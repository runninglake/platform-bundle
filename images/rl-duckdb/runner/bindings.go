package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// BINDINGS: A CREDENTIAL READ IN GO, MADE AN ENGINE SECRET, AND THE ENGINE'S FILES RESTRICTED BEFORE THE LOCK.
//
// The control plane sets RL_BINDINGS_FILE to a JSON file of its own ConfigMap (bindingsFile below): the NAMES and the non-secret shape of each connection the engine is to
// hold a secret for, and what files and URL prefixes the engine may still read. The file holds no value. The value of binding <b> is the file <dir of RL_BINDINGS_FILE>/secrets/<b>, which this
// runner derives itself (a name from a fixed grammar and a fixed directory, so there is no path field to traverse with) and reads in Go.
//
// A DESCRIPTOR PRESENT IS THE TRIGGER FOR TWO THINGS, and a run with none is byte for byte the script this runner always wrote (golden_test.go holds it):
//
//  1. THE RESTRICTION. Before the lock, in the order measured on the pinned engine, the script sets allow_persistent_secrets=false, then allowed_directories (the work directory, the result's
//     directory and what the descriptor's access names), then enable_external_access=false. The other order fails init; allowed_directories alone restricts nothing; and
//     enable_external_access=false alone shuts the work directory too. After the lock the statement can open the work directory, the result's directory and what the descriptor names, and cannot change that.
//     An ABSENT access still restricts (to the work and result directories): the descriptor's presence is the trigger, not its contents.
//  2. THE SECRETS. After the lock and the init sentinel, next to the catalog token's secret, one CREATE SECRET per binding, then the sentinel RL_SECRETS_OK.
//
// THE VALUE IS NEVER A LITERAL OF ITS OWN BYTES. It is rendered as its lower-case hex, decode(from_hex('...')), so a value can never close a string, start a statement or put a
// dot-command on a CLI line; the engine's stdin text for a value stays in [0-9a-f]. Read in Go the value is held exactly: no trim of either end (a trailing newline is part of the
// password), refused by name when it is missing, empty (or only line terminators), over 65536 bytes, not UTF-8, holding a NUL, or unreadable.
//
// A FAILED CREATE SECRET WOULD OTHERWISE PUT THE HEX IN error.log: DuckDB's parser and binder errors quote the statement's text. So when the engine ran init and not the secrets, the
// runner replaces error.log with its own note, and as a belt scans error.log and duckdb.out after any run with a descriptor for a value's bytes or hex.
//
// WHAT IS NOT BUILT HERE. The postgres and mysql types are parsed and validated and then refused type_unavailable: the image carries neither extension, and what their secret takes (its
// parameters, its scope string, its quoting, its TLS word) is unmeasured. The http bearer type exists as the PROOF VEHICLE for everything above and nothing else: it is refused unless the
// runner's own environment says RL_BINDINGS_PROOF=1, which no production path sets. Section 3 of the plan this file implements says why and what each refusal protects.
const (
	bindingsSpecVersion = 1

	// maxBindingsFileBytes bounds the descriptor; sixteen bindings and a short access list are a few KiB.
	maxBindingsFileBytes = 16 << 10
	// maxBindings is agent.MaxResolveBindings, held here by this comment because the repositories are separate.
	maxBindings         = 16
	maxBindingNameBytes = 32
	// maxBindingValueBytes is the resolver's per-binding bound, inclusive; maxBindingValuesBytes is the task's sum.
	maxBindingValueBytes  = 64 << 10
	maxBindingValuesBytes = 256 << 10

	maxAccessEntries    = 16
	maxAccessEntryBytes = 1024
	maxHostBytes        = 253
	maxDatabaseBytes    = 255

	// bindingSecretPrefix is what the plane's later ATTACH names: rl_bind_<binding>. The binding grammar cannot reach rl_catalog_token or another rl_ name.
	bindingSecretPrefix = "rl_bind_"
	// tokenSentinel follows the catalog token's secret when there is a descriptor, so a token that fails is not a binding's failure; secretsSentinel follows the bindings' secrets.
	tokenSentinel   = "RL_TOKEN_OK"
	secretsSentinel = "RL_SECRETS_OK"

	// The rules a refusal names. A refusal says which rule and which binding by its derived name, and nothing of what the file held.
	ruleTypeUnavailable = "type_unavailable"
	ruleValueMissing    = "value_missing"
	ruleValueEmpty      = "value_empty"
	ruleValueTooLarge   = "value_too_large"
	ruleValueNotUTF8    = "value_not_utf8"
	ruleValueHasNUL     = "value_has_nul"
	ruleValueUnreadable = "value_unreadable"
	ruleSecretStatement = "secret_statement_failed"

	// beltMinValueBytes: the belt scan of the logs looks for a value's bytes only from this length (a one-byte password is in every log), and for its hex from twice that.
	beltMinValueBytes = 8
	maxBeltScanBytes  = 16 << 20
)

var (
	reBindingName = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	// reBindingUser is the plane's reSourceUser.
	reBindingUser = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.$-]{0,62}$`)
	// reURLPrefix: an object store only, s3 and gs, scheme, one authority and a path of plain segments, ending in a slash. An abfss prefix waits for a measurement on the image's engine with its extension: the
	// pinned engine read one as a directory relative to the working directory.
	reURLPrefix = regexp.MustCompile(`^(s3|gs)://[A-Za-z0-9][A-Za-z0-9._@-]{0,254}(/[A-Za-z0-9._=-]+)*/$`)
	// reLoopbackPrefix is the one http origin the proof vehicle may be given, and only with RL_BINDINGS_PROOF=1: the loopback stub the real-engine tests start.
	reLoopbackOrigin = regexp.MustCompile(`^http://127\.0\.0\.1:([1-9][0-9]{0,4})/$`)
	reHTTPSScope     = regexp.MustCompile(`^https://[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:([1-9][0-9]{0,4}))?/$`)
	rePortText       = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

	bindingTLSWords = map[string]bool{"require": true, "prefer": true, "verify-full": true, "disable": true}
	// reservedSegments are directories no access entry may be, hold or sit under: the engine's own kernel views and the host's configuration.
	reservedDirs = []string{"/proc/", "/sys/", "/dev/", "/etc/"}
)

// bindingsFile is the descriptor, version 1. Every optional field is a pointer so that a key that is PRESENT is told from one that is absent: `host` is forbidden on an http binding
// whatever it holds. A port is the raw JSON number text, because a decoder that turns a quoted "5432" into a number would make a string a port.
type bindingsFile struct {
	V        int           `json:"v"`
	Access   *bindingsAcc  `json:"access,omitempty"`
	Bindings []bindingDecl `json:"bindings"`
}

type bindingsAcc struct {
	Directories []string `json:"directories"`
	URLPrefixes []string `json:"url_prefixes"`
}

type bindingDecl struct {
	Binding   string          `json:"binding"`
	Type      string          `json:"type"`
	Host      *string         `json:"host,omitempty"`
	Port      json.RawMessage `json:"port,omitempty"`
	Database  *string         `json:"database,omitempty"`
	User      *string         `json:"user,omitempty"`
	TLS       *string         `json:"tls,omitempty"`
	Scope     *string         `json:"scope,omitempty"`
	HTTPProxy *string         `json:"http_proxy,omitempty"`
}

// bindingsShape is the closed set of keys of each object in the descriptor, by the path of the object. Go's decoder matches a key to a field without regard to case and keeps the last of
// two equal keys; neither is a descriptor this runner accepts, so checkBindingsShape walks the tokens first.
var bindingsShape = map[string][]string{
	"":            {"v", "access", "bindings"},
	".access":     {"directories", "url_prefixes"},
	".bindings[]": {"binding", "type", "host", "port", "database", "user", "tls", "scope", "http_proxy"},
}

// checkBindingsShape refuses, before the typed decode, a duplicate key, a key spelled other than exactly as the schema spells it, an unknown key, a null anywhere and a second document.
func checkBindingsShape(buf []byte) error {
	dec := json.NewDecoder(bytes.NewReader(buf))
	if err := walkBindingsValue(dec, "", 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("bindings descriptor holds more than one document")
	}
	return nil
}

func walkBindingsValue(dec *json.Decoder, path string, depth int) error {
	if depth > 4 {
		return errors.New("bindings descriptor is nested deeper than its shape")
	}
	tok, err := dec.Token()
	if err != nil {
		return errors.New("bindings descriptor is not JSON")
	}
	switch t := tok.(type) {
	case nil:
		return errors.New("bindings descriptor holds a null")
	case json.Delim:
		switch t {
		case '{':
			allowed, ok := bindingsShape[path]
			if !ok {
				return errors.New("bindings descriptor holds an object where its shape has none")
			}
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return errors.New("bindings descriptor is not JSON")
				}
				key, _ := kt.(string)
				known := false
				for _, a := range allowed {
					if a == key {
						known = true
					}
				}
				if !known {
					return errors.New("bindings descriptor holds a key this runner does not read")
				}
				if seen[key] {
					return errors.New("bindings descriptor holds a key twice")
				}
				seen[key] = true
				if err := walkBindingsValue(dec, path+"."+key, depth+1); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return errors.New("bindings descriptor is not JSON")
			}
		case '[':
			for dec.More() {
				if err := walkBindingsValue(dec, path+"[]", depth+1); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return errors.New("bindings descriptor is not JSON")
			}
		}
	}
	return nil
}

// protectedPaths are the files and directories the engine must never be given: what the control plane mounted that holds or names a credential.
type protectedPaths struct {
	// secretsDir has a trailing slash. files are the token file and the descriptor, as written and with their symlinks resolved when they can be.
	secretsDir []string
	files      []string
}

func newProtectedPaths(c config) (protectedPaths, error) {
	var p protectedPaths
	desc, err := filepath.Abs(c.bindingsFile)
	if err != nil {
		return p, errors.New("the descriptor's path cannot be made absolute")
	}
	secrets := filepath.Join(filepath.Dir(desc), "secrets")
	for _, s := range withResolved(secrets) {
		p.secretsDir = append(p.secretsDir, strings.TrimRight(s, "/")+"/")
	}
	p.files = append(p.files, withResolved(desc)...)
	if c.catalogTokenFile != "" {
		tok, err := filepath.Abs(c.catalogTokenFile)
		if err != nil {
			return p, errors.New("the catalog token file's path cannot be made absolute")
		}
		p.files = append(p.files, withResolved(tok)...)
	}
	return p, nil
}

// withResolved is a path as written (cleaned) and, when it exists, with its symlinks resolved. DuckDB's own canonicalisation of allowed_directories is unmeasured on the pinned image,
// so a comparison of strings is made against both spellings.
func withResolved(p string) []string {
	p = filepath.Clean(p)
	out := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		out = append(out, r)
	}
	return out
}

// ownDirSpellings is a directory the runner itself opens, absolute and cleaned, followed by the same directory with its symlinks resolved (the last element is the resolved one, and is
// the only element when nothing differs). A directory that does not exist yet is resolved through its nearest existing ancestor, so a result directory the engine creates is still held
// to where it will be.
func ownDirSpellings(dir string) ([]string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	out := []string{abs}
	rest := ""
	for cur := abs; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if res := filepath.Join(r, rest); res != abs {
				out = append(out, res)
			}
			break
		}
		if filepath.Dir(cur) == cur {
			break
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
	return out, nil
}

// overlaps says whether the directory d (cleaned, with a trailing slash) is equal to, above or below the secrets directory, is a prefix of a protected file's path (so / and /run/rl/ are
// refused, and so is /run/rl/secrets/source_1), sits under a protected file, or touches /proc, /sys, /dev or /etc. It compares STRINGS with a trailing slash: /run/rl/par is not /run/rl/params.
func (p protectedPaths) overlaps(d string) bool {
	for _, s := range p.secretsDir {
		if d == s || strings.HasPrefix(s, d) || strings.HasPrefix(d, s) {
			return true
		}
	}
	for _, f := range p.files {
		if strings.HasPrefix(f, d) || strings.HasPrefix(d, f+"/") {
			return true
		}
	}
	for _, r := range reservedDirs {
		// as written and as the host spells it (/etc is /private/etc on a laptop)
		for _, spelled := range withResolved(r) {
			spelled = strings.TrimRight(spelled, "/") + "/"
			if strings.HasPrefix(d, spelled) || strings.HasPrefix(spelled, d) {
				return true
			}
		}
	}
	return false
}

// cleanAccessDir holds one directory of the descriptor's access to its grammar and returns it with exactly one trailing slash, which the RUNNER appends: a plane that forgets one cannot make
// /run/rl/par match /run/rl/params. Absolute, no . or .. segment, no empty segment, no control byte, no whitespace.
func cleanAccessDir(d string) (string, error) {
	if d == "" || d[0] != '/' || len(d) > maxAccessEntryBytes || !validPlainText(d) {
		return "", errors.New("an access directory is not an absolute path of plain characters")
	}
	segs := strings.Split(strings.TrimPrefix(d, "/"), "/")
	if segs[len(segs)-1] == "" {
		segs = segs[:len(segs)-1]
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", errors.New("an access directory has an empty, . or .. segment")
		}
	}
	if len(segs) == 0 {
		return "/", nil
	}
	return "/" + strings.Join(segs, "/") + "/", nil
}

// validPlainText: valid UTF-8, no replacement character (what the decoder writes for a byte it could not read), no control character, no whitespace of any script.
func validPlainText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// cleanURLPrefix holds one object-store prefix to its grammar. NO http(s) PREFIX IN THIS VERSION: the plane's catalog endpoint is reached by REST calls that are not file-system calls, so it
// needs no entry, and an entry for it would let a statement read any URL under the catalog service. The one exception is the proof vehicle's loopback stub, and only with RL_BINDINGS_PROOF=1.
func cleanURLPrefix(p string, proof bool) error {
	if len(p) > maxAccessEntryBytes {
		return errors.New("an access url prefix is too long")
	}
	if reURLPrefix.MatchString(p) {
		for _, seg := range strings.Split(strings.TrimPrefix(p[strings.Index(p, "://")+3:], "/"), "/") {
			if seg == "." || seg == ".." {
				return errors.New("an access url prefix has a . or .. segment")
			}
		}
		return nil
	}
	if proof {
		if m := reLoopbackOrigin.FindStringSubmatch(p); m != nil && portInRange(m[1]) {
			return nil
		}
	}
	return errors.New("an access url prefix is not an object-store prefix this runner accepts")
}

func portInRange(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// bindingsDescriptor is a descriptor that passed every check, with its access already cleaned.
type bindingsDescriptor struct {
	bindings []bindingDecl
	dirs     []string
	prefixes []string
}

func (d *bindingsDescriptor) names() []string {
	out := make([]string, len(d.bindings))
	for i, b := range d.bindings {
		out[i] = b.Binding
	}
	return out
}

// loadBindings reads the descriptor and holds it to the contract. Any fault is the caller's invalid-query class: a descriptor this runner cannot understand is refused before the engine
// runs, never run as a query. The error never carries a value from the file, a name included.
func loadBindings(c config) (*bindingsDescriptor, error) {
	f, err := os.Open(c.bindingsFile)
	if err != nil {
		return nil, errors.New("bindings descriptor: cannot be opened")
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("bindings descriptor: not a regular file")
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxBindingsFileBytes+1))
	if err != nil {
		return nil, errors.New("bindings descriptor: cannot be read")
	}
	if len(buf) > maxBindingsFileBytes {
		return nil, fmt.Errorf("bindings descriptor exceeds %d bytes", maxBindingsFileBytes)
	}
	// Invalid UTF-8 is rewritten by the decoder to the replacement character and would pass as text; it is refused before the decoder sees it.
	if !utf8.Valid(buf) {
		return nil, errors.New("bindings descriptor is not UTF-8")
	}
	if err := checkBindingsShape(buf); err != nil {
		return nil, err
	}
	var bf bindingsFile
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bf); err != nil {
		return nil, errors.New("bindings descriptor is not JSON of the shape this runner reads")
	}
	if bf.V != bindingsSpecVersion {
		return nil, fmt.Errorf("bindings descriptor: version %d is not the %d this runner reads", bf.V, bindingsSpecVersion)
	}
	if bf.Bindings == nil {
		return nil, errors.New("bindings descriptor: bindings is a list, empty or not")
	}
	if len(bf.Bindings) > maxBindings {
		return nil, fmt.Errorf("bindings descriptor: more than %d bindings", maxBindings)
	}
	seen := map[string]bool{}
	for i := range bf.Bindings {
		if err := bf.Bindings[i].validate(i); err != nil {
			return nil, err
		}
		if seen[bf.Bindings[i].Binding] {
			return nil, fmt.Errorf("bindings descriptor: binding %d repeats an earlier binding's name", i)
		}
		seen[bf.Bindings[i].Binding] = true
	}
	d := &bindingsDescriptor{bindings: bf.Bindings}
	sort.Slice(d.bindings, func(i, j int) bool { return d.bindings[i].Binding < d.bindings[j].Binding })
	if bf.Access != nil {
		prot, err := newProtectedPaths(c)
		if err != nil {
			return nil, fmt.Errorf("bindings descriptor: %v", err)
		}
		if len(bf.Access.Directories) > maxAccessEntries || len(bf.Access.URLPrefixes) > maxAccessEntries {
			return nil, fmt.Errorf("bindings descriptor: more than %d access entries of a kind", maxAccessEntries)
		}
		for _, raw := range bf.Access.Directories {
			dir, err := cleanAccessDir(raw)
			if err != nil {
				return nil, fmt.Errorf("bindings descriptor: %v", err)
			}
			if prot.overlaps(dir) {
				return nil, errors.New("bindings descriptor: an access directory overlaps a file the runner protects")
			}
			// A directory that is a symlink to a protected one is the same directory.
			if r, err := filepath.EvalSymlinks(dir); err == nil && prot.overlaps(strings.TrimRight(r, "/")+"/") {
				return nil, errors.New("bindings descriptor: an access directory overlaps a file the runner protects")
			}
			d.dirs = append(d.dirs, dir)
		}
		for _, p := range bf.Access.URLPrefixes {
			if err := cleanURLPrefix(p, c.bindingsProof); err != nil {
				return nil, fmt.Errorf("bindings descriptor: %v", err)
			}
			d.prefixes = append(d.prefixes, p)
		}
	}
	return d, nil
}

func (b *bindingDecl) validate(i int) error {
	bad := func(what string) error { return fmt.Errorf("bindings descriptor: binding %d: %s", i, what) }
	if !reBindingName.MatchString(b.Binding) || len(b.Binding) > maxBindingNameBytes || b.Binding == "catalog_token" {
		return bad("the name is not a binding name")
	}
	for _, s := range []*string{b.Host, b.Database, b.User, b.TLS, b.Scope, b.HTTPProxy} {
		if s != nil && strings.ContainsRune(*s, utf8.RuneError) {
			return bad("a field holds a character this runner does not read")
		}
	}
	switch b.Type {
	case "postgres", "mysql":
		if b.Scope != nil || b.HTTPProxy != nil {
			return bad("scope and http_proxy are for the http type only")
		}
		if b.Host == nil || b.Database == nil || b.User == nil || b.Port == nil {
			return bad("host, port, database and user are required")
		}
		ok := hostOK(*b.Host)
		if b.Type == "mysql" {
			ok = mysqlHostOK(*b.Host)
		}
		if !ok {
			return bad("the host is not a host name or address")
		}
		if !rePortText.MatchString(string(b.Port)) || !portInRange(string(b.Port)) {
			return bad("the port is not a whole number from 1 to 65535")
		}
		if !databaseOK(*b.Database) {
			return bad("the database is not a name this runner accepts")
		}
		if !reBindingUser.MatchString(*b.User) {
			return bad("the user is not a user name this runner accepts")
		}
		if b.TLS != nil && !bindingTLSWords[*b.TLS] {
			return bad("tls is not one of the four words")
		}
	case "http":
		if b.Host != nil || b.Port != nil || b.Database != nil || b.User != nil || b.TLS != nil {
			return bad("host, port, database, user and tls are not for the http type")
		}
		if b.Scope == nil || !scopeOK(*b.Scope) {
			return bad("the scope is not an origin ending in a slash")
		}
		// REQUIRED, never defaulted: a scoped http secret with no proxy value goes direct past the plane's unscoped proxy secret.
		if b.HTTPProxy == nil || !proxyOK(*b.HTTPProxy) {
			return bad("http_proxy is not an address and port or the word direct")
		}
	default:
		return bad("the type is not one this runner knows")
	}
	return nil
}

// hostOK is sourcehost.HostOK: a DNS name, an IPv4 or an IPv6 address, bare, no underscore. A host with a colon must parse as an address without a zone.
func hostOK(h string) bool {
	if h == "" || len(h) > maxHostBytes || h[0] == '-' || h[0] == '.' {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == ':', c == '-':
		default:
			return false
		}
	}
	if strings.Contains(h, ":") {
		a, err := netip.ParseAddr(h)
		return err == nil && a.Zone() == ""
	}
	return true
}

// mysqlHostOK is sourcehost.MySQLHostOK: ASCII letters, digits, underscore, hyphen and dot, not starting with a dot or a hyphen. No colon, so no IPv6 literal.
func mysqlHostOK(h string) bool {
	if h == "" || len(h) > maxHostBytes {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return h[0] != '-' && h[0] != '.'
}

// databaseOK is sourcehost.DatabaseOK: valid UTF-8, no control, format (zero-width, bidirectional) or line- or paragraph-separator character, none of the address syntax characters
// (a name is connection-string syntax in some drivers), not . or .., at most 255 bytes.
func databaseOK(d string) bool {
	if d == "" || len(d) > maxDatabaseBytes || !utf8.ValidString(d) || d == "." || d == ".." || strings.ContainsAny(d, "?&#/%;+ ") {
		return false
	}
	for _, r := range d {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

// scopeOK: an https origin, or the loopback stub's http origin (http type is itself proof-only), each ending in a slash because a scope matches by string prefix.
func scopeOK(s string) bool {
	if len(s) > maxHostBytes+16 {
		return false
	}
	if m := reHTTPSScope.FindStringSubmatch(s); m != nil {
		return m[3] == "" || portInRange(m[3])
	}
	if m := reLoopbackOrigin.FindStringSubmatch(s); m != nil {
		return portInRange(m[1])
	}
	return false
}

// proxyOK: an address and port the engine's proxy setting takes, or exactly the word direct (the explicit form for the loopback stub; the plane never sends it).
func proxyOK(s string) bool {
	if s == "direct" {
		return true
	}
	ap, err := netip.ParseAddrPort(s)
	return err == nil && ap.Addr().Zone() == "" && ap.Port() >= 1 && ap.String() == s
}

// bindingTypeAvailable says whether this runner can make a secret of the type. postgres and mysql are not: the extensions are not in the image. http is the proof vehicle's and only
// with RL_BINDINGS_PROOF=1.
func bindingTypeAvailable(typ string, proof bool) bool {
	return typ == "http" && proof
}

// bindingValue is one binding's value as the runner read it. The bytes are never turned into a string; wipe zeroes them.
type bindingValue struct {
	name  string
	value []byte
}

// valueRefusal is a refusal of a value: the rule's name and the binding's name, never a byte of what the file held.
type valueRefusal struct{ rule, binding string }

func (r valueRefusal) Error() string { return fmt.Sprintf("%s (binding %s)", r.rule, r.binding) }

// readBindingValue reads <secretsDir>/<name> exactly: no trim of either end. A value that is empty, or only line terminators, is empty (a file holding only a newline is refused as empty); a
// value with a trailing newline and other bytes is kept whole.
func readBindingValue(secretsDir, name string) ([]byte, error) {
	refuse := func(rule string) error { return valueRefusal{rule, name} }
	f, err := os.Open(filepath.Join(secretsDir, name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, refuse(ruleValueMissing)
		}
		return nil, refuse(ruleValueUnreadable)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, refuse(ruleValueUnreadable)
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxBindingValueBytes+1))
	if err != nil {
		return nil, refuse(ruleValueUnreadable)
	}
	switch {
	case len(buf) > maxBindingValueBytes:
		return nil, refuse(ruleValueTooLarge)
	case len(bytes.Trim(buf, "\r\n")) == 0:
		return nil, refuse(ruleValueEmpty)
	case !utf8.Valid(buf):
		return nil, refuse(ruleValueNotUTF8)
	case bytes.IndexByte(buf, 0) >= 0:
		return nil, refuse(ruleValueHasNUL)
	}
	return buf, nil
}

// bindingsPlan is a descriptor that passed, with the values read and the list of what the engine may still read.
type bindingsPlan struct {
	desc   *bindingsDescriptor
	values []bindingValue
	// allowed is allowed_directories, in order: the work directory, the result's directory, the descriptor's directories (each with its slash) and its url prefixes.
	allowed []string
}

func (p *bindingsPlan) wipe() {
	for _, v := range p.values {
		for i := range v.value {
			v.value[i] = 0
		}
	}
}

// secretsDirOf is where a binding's value is: beside the descriptor, in secrets.
func secretsDirOf(bindingsFile string) string {
	return filepath.Join(filepath.Dir(bindingsFile), "secrets")
}

// prepareBindings loads the descriptor, refuses what this runner cannot make and reads the values. It returns the class a refusal is reported as: a descriptor it cannot understand is the
// plane's fault (invalid_query_file); a type it cannot make and a value it will not use are binding_refused.
func prepareBindings(c config) (*bindingsPlan, string, error) {
	d, err := loadBindings(c)
	if err != nil {
		return nil, classInvalidQuery, err
	}
	p := &bindingsPlan{desc: d}
	prot, err := newProtectedPaths(c)
	if err != nil {
		return nil, classInvalidQuery, fmt.Errorf("bindings descriptor: %v", err)
	}
	// The runner's own directories come first, and they too must not reach a protected file: a result file at the root would put / in the list. Each is made absolute and
	// symlink-resolved FIRST, because the engine resolves both an allowed directory and the path it is asked to open: a relative spelling, or a link to the descriptor's directory, would
	// pass a comparison of strings and still name the protected directory. Both spellings are checked, and the resolved one is what the list carries.
	var own []string
	for _, dir := range []string{c.workDir, filepath.Dir(c.resultFile)} {
		spellings, err := ownDirSpellings(dir)
		if err != nil {
			return nil, classInvalidQuery, errors.New("bindings descriptor: the work or result directory cannot be made absolute")
		}
		for _, sp := range spellings {
			if prot.overlaps(strings.TrimRight(sp, "/") + "/") {
				return nil, classInvalidQuery, errors.New("bindings descriptor: the work or result directory overlaps a file the runner protects")
			}
		}
		own = append(own, spellings[len(spellings)-1])
	}
	add := func(s string) {
		for _, have := range p.allowed {
			if have == s {
				return
			}
		}
		p.allowed = append(p.allowed, s)
	}
	for _, s := range own {
		add(s)
	}
	for _, s := range d.dirs {
		add(s)
	}
	for _, s := range d.prefixes {
		add(s)
	}
	for _, b := range d.bindings {
		if !bindingTypeAvailable(b.Type, c.bindingsProof) {
			return nil, classBindingRefused, valueRefusal{ruleTypeUnavailable, b.Binding}
		}
	}
	total := 0
	for _, b := range d.bindings {
		v, err := readBindingValue(secretsDirOf(c.bindingsFile), b.Binding)
		if err != nil {
			p.wipe()
			return nil, classBindingRefused, err
		}
		p.values = append(p.values, bindingValue{b.Binding, v})
		if total += len(v); total > maxBindingValuesBytes {
			p.wipe()
			return nil, classBindingRefused, valueRefusal{ruleValueTooLarge, b.Binding}
		}
	}
	return p, "", nil
}

// restrictionSQL is the three settings, in the order measured on the pinned engine, ending just before the lock.
func (p *bindingsPlan) restrictionSQL() string {
	quoted := make([]string, len(p.allowed))
	for i, a := range p.allowed {
		quoted[i] = sqlString(a)
	}
	return "SET allow_persistent_secrets=false;\n" +
		"SET allowed_directories=[" + strings.Join(quoted, ", ") + "];\n" +
		"SET enable_external_access=false;\n"
}

// writeSecrets writes, after the lock, one CREATE SECRET per binding in name order and then the sentinel. The value is written as hex directly into the buffer and never as a Go string.
func (p *bindingsPlan) writeSecrets(b *bytes.Buffer) {
	for i, d := range p.desc.bindings {
		renderBinding(b, d, p.values[i].value)
	}
	b.WriteString("SELECT '" + secretsSentinel + "';\n")
}

// renderBinding writes one binding's CREATE SECRET into b, the value as its hex. Only a type this runner can make has a renderer: postgres and mysql have none, and a descriptor that
// names one never gets this far (bindingTypeAvailable).
func renderBinding(b *bytes.Buffer, d bindingDecl, value []byte) {
	switch d.Type {
	case "http":
		proxy := *d.HTTPProxy
		if proxy == "direct" {
			proxy = ""
		}
		b.WriteString("CREATE SECRET " + bindingSecretPrefix + d.Binding + " (TYPE http, BEARER_TOKEN decode(from_hex('")
		hx := make([]byte, hex.EncodedLen(len(value)))
		hex.Encode(hx, value)
		b.Write(hx)
		for i := range hx {
			hx[i] = 0
		}
		b.WriteString("')), SCOPE " + sqlString(*d.Scope) + ", HTTP_PROXY " + sqlString(proxy) + ");\n")
	}
}

// bindingsAck is the RL_BINDINGS line: which secrets the engine made, and whether the run's files were restricted, both read from the sentinels the engine printed and not from what
// the script was meant to do.
type bindingsAck struct {
	V          int      `json:"v"`
	Made       []string `json:"made"`
	Restricted bool     `json:"restricted"`
}

// settle reads the sentinels of a finished run into the ack. restricted is true only when the engine PRINTED the init sentinel, which a script carrying the three settings reaches only
// after they passed; made is every binding's name when the engine printed the secrets sentinel and none otherwise.
func (p *bindingsPlan) settle(outPath string) *bindingsAck {
	a := &bindingsAck{V: bindingsSpecVersion, Made: []string{}, Restricted: initRan(outPath)}
	if a.Restricted && sentinelRan(outPath, secretsSentinel) {
		a.Made = p.desc.names()
	}
	return a
}

// scrubNote is what replaces a log that could hold a value's text: the rule and the bindings' derived names, never a value, its hex, a host, a database, a user or a scope.
func (p *bindingsPlan) scrubNote() string {
	return fmt.Sprintf("rl-duckdb-runner: binding_refused: %s (bindings %s); the engine's own log of it was replaced, because an engine error quotes the statement\n", ruleSecretStatement, strings.Join(p.desc.names(), ","))
}

// beltScan replaces error.log and duckdb.out with the runner's note when either holds a value's bytes or its hex. A BELT, NOT THE FENCE: the fence is that the secret's text is never in
// a log that was written after a failure of it. Values shorter than beltMinValueBytes are not looked for (a short password is in every log).
func (p *bindingsPlan) beltScan(errorLog, outPath string) {
	for _, path := range []string{errorLog, outPath} {
		content := readHeadBytes(path, maxBeltScanBytes)
		if len(content) == 0 {
			continue
		}
		lower := bytes.ToLower(content)
		for _, v := range p.values {
			if len(v.value) < beltMinValueBytes {
				continue
			}
			hx := make([]byte, hex.EncodedLen(len(v.value)))
			hex.Encode(hx, v.value)
			found := bytes.Contains(content, v.value) || bytes.Contains(lower, hx)
			for i := range hx {
				hx[i] = 0
			}
			if found {
				_ = os.WriteFile(path, []byte(p.scrubNote()), 0o600)
				break
			}
		}
		for i := range content {
			content[i] = 0
		}
	}
}

func readHeadBytes(path string, n int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, int64(n)))
	return b
}
