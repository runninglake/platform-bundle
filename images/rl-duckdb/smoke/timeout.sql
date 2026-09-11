SELECT sum(a.i * b.i) FROM range(10000000) a(i), range(10000000) b(i)
