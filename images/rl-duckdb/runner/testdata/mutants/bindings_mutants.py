#!/usr/bin/env python3
"""Scripted mutants of the bindings code: each breaks one rule on purpose, and the whole test suite must go red on a named test.

Run from images/rl-duckdb/runner:   python3 testdata/mutants/bindings_mutants.py [id ...]

A mutant is a list of (file, old text, new text). The text must occur exactly once. The script restores every file it touched (from the bytes it read, never from git) after each run, and also
if it is interrupted. A mutant that does not compile is reported INVALID and does not count as killed. Exit status 1 when any mutant survives.
"""
import os
import re
import subprocess
import sys

M = []


def mut(mid, why, *edits):
    M.append((mid, why, edits))


MAIN, B = "main.go", "bindings.go"

# ---- the restriction -----------------------------------------------------------------------------------------------------------------------------------------
mut("M01", "drop the restriction", (MAIN, "\t\tb.WriteString(cfg.bindings.restrictionSQL())\n", "\t\t_ = cfg.bindings.restrictionSQL()\n"))
mut("M02", "the restriction after the lock", (MAIN, "\tif cfg.bindings != nil {\n\t\tb.WriteString(cfg.bindings.restrictionSQL())\n\t}\n", ""),
    (MAIN, "\tfmt.Fprintf(&b, \"SELECT '%s';\\n\", initSentinel)\n", "\tfmt.Fprintf(&b, \"SELECT '%s';\\n\", initSentinel)\n\tif cfg.bindings != nil {\n\t\tb.WriteString(cfg.bindings.restrictionSQL())\n\t}\n"))
mut("M03", "the two settings in the wrong order", (B, '''"SET allowed_directories=[" + strings.Join(quoted, ", ") + "];\\n" +
		"SET enable_external_access=false;\\n"''', '''"SET enable_external_access=false;\\n" +
		"SET allowed_directories=[" + strings.Join(quoted, ", ") + "];\\n"'''))
mut("M04", "external access off and no allowed directories", (B, '''"SET allowed_directories=[" + strings.Join(quoted, ", ") + "];\\n" +''', '''""+'''))
mut("M05", "persistent secrets left on", (B, '''return "SET allow_persistent_secrets=false;\\n" +''', '''return ""+'''))
mut("M06", "an absent access is no restriction", (B, "func (p *bindingsPlan) restrictionSQL() string {\n", "func (p *bindingsPlan) restrictionSQL() string {\n\tif len(p.desc.dirs) == 0 && len(p.desc.prefixes) == 0 {\n\t\treturn \"\"\n\t}\n"))
mut("M07", "the work directory left out of the allowed list", (B, "\tadd(work)\n", ""))
mut("M08", "the descriptor's own directories left out", (B, "\tfor _, s := range d.dirs {\n\t\tadd(s)\n\t}\n", ""))

# ---- the value -----------------------------------------------------------------------------------------------------------------------------------------------
mut("M09", "the value trimmed", (B, "\treturn buf, nil\n}\n\n// bindingsPlan is", "\treturn bytes.TrimSpace(buf), nil\n}\n\n// bindingsPlan is"))
mut("M10", "a newline-only value accepted", (B, 'case len(bytes.Trim(buf, "\\r\\n")) == 0:', "case len(buf) == 0:"))
mut("M11", "the value rendered as a literal of its own bytes", (B, "\t\tb.Write(hx)\n", "\t\tb.Write(value)\n"))
mut("M12", "a NUL accepted", (B, "\tcase bytes.IndexByte(buf, 0) >= 0:\n", "\tcase false:\n"))
mut("M13", "UTF-8 not checked", (B, "\tcase !utf8.Valid(buf):\n", "\tcase false:\n"))
mut("M14", "the per-value bound off by one", (B, "case len(buf) > maxBindingValueBytes:", "case len(buf) > maxBindingValueBytes+1:"))
mut("M15", "the sum cap dropped", (B, "if total += len(v); total > maxBindingValuesBytes {", "if total += len(v); false {"))
mut("M16", "a refused value reported as an invalid descriptor", (B, "\t\t\tp.wipe()\n\t\t\treturn nil, classBindingRefused, err\n", "\t\t\tp.wipe()\n\t\t\treturn nil, classInvalidQuery, err\n"))

# ---- the validator -------------------------------------------------------------------------------------------------------------------------------------------
mut("M17", "the overlap check dropped", (B, "\t\t\tif prot.overlaps(dir) {\n", "\t\t\tif false && prot.overlaps(dir) {\n"))
mut("M18", "a directory not given its slash", (B, '''return "/" + strings.Join(segs, "/") + "/", nil''', '''return "/" + strings.Join(segs, "/"), nil'''))
mut("M19", "a .. segment accepted", (B, 's == "" || s == "." || s == ".."', 's == "" || s == "."'))
mut("M20", "symlinks of a protected path not resolved", (B, "if r, err := filepath.EvalSymlinks(p); err == nil && r != p {", "if r, err := filepath.EvalSymlinks(p); false && err == nil && r != p {"))
mut("M21", "a symlinked entry not resolved", (B, "if r, err := filepath.EvalSymlinks(dir); err == nil && prot.overlaps", "if r, err := filepath.EvalSymlinks(dir); false && err == nil && prot.overlaps"))
mut("M22", "an https url prefix accepted", (B, "(s3|gs|abfss)://", "(s3|gs|abfss|https?)://"))
mut("M23", "the loopback prefix accepted without the proof word", (B, "\tif proof {\n\t\tif m := reLoopbackOrigin", "\tif true {\n\t\tif m := reLoopbackOrigin"))
mut("M24", "http_proxy optional", (B, "if b.HTTPProxy == nil || !proxyOK(*b.HTTPProxy) {", "if b.HTTPProxy != nil && !proxyOK(*b.HTTPProxy) {"))
mut("M25", "duplicate keys accepted", (B, "\t\t\t\tif seen[key] {\n", "\t\t\t\tif false && seen[key] {\n"))
mut("M26", "keys matched without regard to case", (B, "\t\t\t\t\tif a == key {\n", "\t\t\t\t\tif strings.EqualFold(a, key) {\n"))
mut("M27", "a port written as text accepted", (B, "rePortText       = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)", "rePortText       = regexp.MustCompile(`^\"?[1-9][0-9]{0,4}\"?$`)"),
    (B, "\tn, err := strconv.Atoi(s)\n", "\tn, err := strconv.Atoi(strings.Trim(s, \"\\\"\"))\n"))
mut("M28", "catalog_token accepted as a binding name", (B, ' || b.Binding == "catalog_token"', ""))
mut("M29", "the descriptor's size cap off", (B, "len(buf) > maxBindingsFileBytes {", "len(buf) > 10*maxBindingsFileBytes {"))
mut("M30", "the work and result directories not held to the overlap check", (B, '\t\tif prot.overlaps(strings.TrimRight(filepath.Clean(own), "/") + "/") {', '\t\tif false && prot.overlaps(strings.TrimRight(filepath.Clean(own), "/") + "/") {'))
mut("M31", "a postgres host accepts an underscore", (B, "\t\tok := hostOK(*b.Host)\n", "\t\tok := mysqlHostOK(*b.Host)\n"))
mut("M32", "the database's address syntax accepted", (B, 'strings.ContainsAny(d, "?&#/%;+ ")', 'strings.ContainsAny(d, "")'))
mut("M33", "an empty user accepted", (B, "if !reBindingUser.MatchString(*b.User) {", "if false && !reBindingUser.MatchString(*b.User) {"))

# ---- the types and the proof word --------------------------------------------------------------------------------------------------------------------------
mut("M34", "postgres accepted", (B, 'return typ == "http" && proof', 'return (typ == "http" || typ == "postgres") && proof'))
mut("M35", "http accepted without RL_BINDINGS_PROOF", (B, 'return typ == "http" && proof', 'return typ == "http"'))

# ---- the modes -----------------------------------------------------------------------------------------------------------------------------------------------
mut("M36", "a probe accepted with a descriptor", (MAIN, '\t\tcase cfg.probeFile != "":\n\t\t\tnote("a probe has no bindings")', '\t\tcase false:\n\t\t\tnote("a probe has no bindings")'))
mut("M37", "a bare statement accepted with a descriptor", (MAIN, '\t\tcase kind == kindStatement:\n\t\t\tnote("bindings are not', '\t\tcase false:\n\t\t\tnote("bindings are not'))
mut("M38", "byte measuring accepted with a descriptor", (MAIN, '\t\tcase cfg.measureBytes:\n\t\t\tnote("bindings are not', '\t\tcase false:\n\t\t\tnote("bindings are not'))

# ---- the ack and the sentinels ----------------------------------------------------------------------------------------------------------------------------
mut("M39", "restricted always true", (B, "Restricted: initRan(outPath)}", "Restricted: true}"))
mut("M40", "made always the names", (B, "if a.Restricted && sentinelRan(outPath, secretsSentinel) {", "if a.Restricted {"))
mut("M41", "the ack printed after the task's claim", (MAIN, '''	if m.bindings != nil {
		if bb, err := json.Marshal(m.bindings); err == nil {
			fmt.Fprintf(w, "RL_BINDINGS %s\\n", bb)
		}
	}
''', ""),
    (MAIN, '''	b, err := json.Marshal(m)
	if err != nil {
		// A struct of numbers''', '''	if m.bindings != nil {
		if bb, err := json.Marshal(m.bindings); err == nil {
			fmt.Fprintf(w, "RL_BINDINGS %s\\n", bb)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		// A struct of numbers'''))
mut("M42", "the secrets sentinel read as the init sentinel", (MAIN, "if m.bindings.Restricted && !sentinelRan(outPath, secretsSentinel) {", "if m.bindings.Restricted && !sentinelRan(outPath, initSentinel) {"))
mut("M43", "a failed token statement classed as a binding's", (MAIN, 'if cfg.catalogToken != "" && !sentinelRan(outPath, tokenSentinel) {', 'if false && cfg.catalogToken != "" && !sentinelRan(outPath, tokenSentinel) {'))
mut("M44", "the token sentinel written without a descriptor", (MAIN, "\t\tif cfg.bindings != nil {\n\t\t\tfmt.Fprintf(&b, \"SELECT '%s';\\n\", tokenSentinel)\n\t\t}\n", "\t\t{\n\t\t\tfmt.Fprintf(&b, \"SELECT '%s';\\n\", tokenSentinel)\n\t\t}\n"))
mut("M45", "a failure between the sentinels classed as the statement's", (MAIN, "\t\t\treturn finish(classBindingRefused, 1)\n\t\t}\n", "\t\t}\n"))

# ---- the log -------------------------------------------------------------------------------------------------------------------------------------------------
mut("M46", "error.log not replaced after a failed secret statement", (MAIN, "\t\t\t_ = logf.Truncate(0)\n\t\t\tfmt.Fprint(logf, cfg.bindings.scrubNote())\n", "\t\t\tfmt.Fprint(logf, cfg.bindings.scrubNote())\n"))
mut("M47", "the note carries the hex", (B, '''strings.Join(p.desc.names(), ","))''', '''strings.Join(p.desc.names(), ",")+hex.EncodeToString(p.values[0].value))'''))
mut("M48", "the belt dropped", (B, "func (p *bindingsPlan) beltScan(errorLog, outPath string) {\n", "func (p *bindingsPlan) beltScan(errorLog, outPath string) {\n\treturn\n"))
mut("M49", "the values not zeroed", (B, "\t\t\tv.value[i] = 0\n", "\t\t\t_ = i\n"))
mut("M50", "the secrets statements dropped", (B, "\t\trenderBinding(b, d, p.values[i].value)\n", "\t\t_, _ = d, i\n"))


def run(args):
    ids = set(args)
    originals = {}
    survivors = []
    lines = []
    try:
        for mid, why, edits in M:
            if ids and mid not in ids:
                continue
            touched = {}
            ok = True
            for f, old, new in edits:
                src = touched.get(f)
                if src is None:
                    src = open(f).read()
                    originals.setdefault(f, src)
                if src.count(old) != 1:
                    lines.append("%s  %s: INVALID (the text to replace occurs %d times in %s)" % (mid, why, src.count(old), f))
                    print(lines[-1], flush=True)
                    ok = False
                    break
                touched[f] = src.replace(old, new)
            if not ok:
                continue
            try:
                for f, src in touched.items():
                    open(f, "w").write(src)
                p = subprocess.run(["go", "test", "-count=1", "-timeout", "300s", "."], capture_output=True, text=True, timeout=400)
                out = p.stdout + p.stderr
            except subprocess.TimeoutExpired:
                out = "--- FAIL: (timeout)"
                p = None
            finally:
                for f in touched:
                    open(f, "w").write(originals[f])
            if "[build failed]" in out or "[setup failed]" in out:
                lines.append("%s  %s: INVALID (does not compile)\n%s" % (mid, why, out[:600]))
                print(lines[-1], flush=True)
                continue
            failed = re.findall(r"^--- FAIL: (\S+)", out, re.M)
            if failed:
                lines.append("%s  %s: KILLED by %s" % (mid, why, ", ".join(failed)))
            else:
                lines.append("%s  %s: SURVIVED" % (mid, why))
                survivors.append(mid)
            print(lines[-1], flush=True)
    finally:
        for f, src in originals.items():
            open(f, "w").write(src)
    print("\n%d mutants run, %d survived: %s" % (len(lines), len(survivors), " ".join(survivors) or "none"))
    return 1 if survivors else 0


if __name__ == "__main__":
    sys.exit(run(sys.argv[1:]))
