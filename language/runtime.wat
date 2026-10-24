;; The runtime of compiled scripts: memory, the collector, handles, strings, arrays and maps. The code generator
;; (gen.go) emits only the functions a script uses; $NAME constants and globals are its (abi.go).
;;
;; Every block on the heap starts with a 16-byte header: its tag (the mark bit is the high bit), its size in
;; bytes (header included, a multiple of 8), the forwarding address the collector computes, and a word of its
;; own: a string's byte length, an array's length, a map's count, a store's capacity. Values take 8-byte slots.
;;
;;   string     +16 the UTF-8 bytes
;;   array      +16 the store of its elements (8 bytes each), +12 its length
;;   map        +16 keys store, +20 values store, +24 index (i32 entries: 0 empty, else entry + 1),
;;              +28 flags (1: string keys, 2: reference values), +12 the number of entries
;;   object     +16 its fields, 8 bytes each; a native class's object holds the host's handle at +16
;;   handles    +16 i32 entries: objects the host holds, odd entries are free (the next free one, shifted)
;;   natives    +16 i32 entries: native objects by the host's handle, held weakly

;; ---- errors: each writes its arguments into the scratch slots and tells the host ----

(func $fail_steps (param $pos i32)
  (i32.store offset=16 (i32.const 0) (i32.const $MAX_STEPS))
  (call $imp_fail (i32.const $E_STEPS) (local.get $pos))
  unreachable)

(func $fail_depth (param $pos i32)
  (i32.store offset=16 (i32.const 0) (i32.const $MAX_DEPTH))
  (call $imp_fail (i32.const $E_DEPTH) (local.get $pos))
  unreachable)

(func $fail_memory (param $pos i32)
  (i32.store offset=16 (i32.const 0) (i32.const $MAX_HEAP))
  (call $imp_fail (i32.const $E_MEMORY) (local.get $pos))
  unreachable)

(func $fail_length (param $what i32) (param $pos i32)
  (i32.store offset=16 (i32.const 0) (local.get $what))
  (i32.store offset=24 (i32.const 0) (i32.const $MAX_LENGTH))
  (call $imp_fail (i32.const $E_LENGTH) (local.get $pos))
  unreachable)

(func $fail_string (param $pos i32)
  (i32.store offset=16 (i32.const 0) (i32.const $MAX_STRING))
  (call $imp_fail (i32.const $E_STRING) (local.get $pos))
  unreachable)

(func $fail_divzero (param $pos i32)
  (call $imp_fail (i32.const $E_DIVZERO) (local.get $pos))
  unreachable)

(func $fail_large (param $op i32) (param $pos i32)
  (i32.store offset=16 (i32.const 0) (local.get $op))
  (call $imp_fail (i32.const $E_LARGE) (local.get $pos))
  unreachable)

(func $fail_index (param $i f64) (param $n i32) (param $pos i32)
  (f64.store offset=16 (i32.const 0) (local.get $i))
  (i32.store offset=24 (i32.const 0) (local.get $n))
  (call $imp_fail (i32.const $E_INDEX) (local.get $pos))
  unreachable)

(func $fail_index_set (param $i i32) (param $n i32) (param $pos i32)
  (i32.store offset=16 (i32.const 0) (local.get $i))
  (i32.store offset=24 (i32.const 0) (local.get $n))
  (call $imp_fail (i32.const $E_INDEX_SET) (local.get $pos))
  unreachable)

(func $fail_null (param $name i32) (param $pos i32)
  (i32.store offset=16 (i32.const 0) (local.get $name))
  (call $imp_fail (i32.const $E_NULL) (local.get $pos))
  unreachable)

(func $fail_null_write (param $name i32) (param $type i32) (param $pos i32)
  (i32.store offset=16 (i32.const 0) (local.get $name))
  (i32.store offset=24 (i32.const 0) (local.get $type))
  (call $imp_fail (i32.const $E_NULL_WRITE) (local.get $pos))
  unreachable)

(func $fail_pop (param $pos i32)
  (call $imp_fail (i32.const $E_POP) (local.get $pos))
  unreachable)

(func $fail_slice (param $n f64) (param $pos i32)
  (f64.store offset=16 (i32.const 0) (local.get $n))
  (call $imp_fail (i32.const $E_SLICE) (local.get $pos))
  unreachable)

(func $fail_notnum (param $name i32) (param $s i32) (param $pos i32)
  (i32.store offset=16 (i32.const 0) (local.get $name))
  (i32.store offset=24 (i32.const 0) (local.get $s))
  (call $imp_fail (i32.const $E_NOT_NUMBER) (local.get $pos))
  unreachable)

(func $fail_notbool (param $name i32) (param $s i32) (param $pos i32)
  (i32.store offset=16 (i32.const 0) (local.get $name))
  (i32.store offset=24 (i32.const 0) (local.get $s))
  (call $imp_fail (i32.const $E_NOT_BOOL) (local.get $pos))
  unreachable)

(func $fail_hostfield (param $name i32) (param $pos i32)
  (i32.store offset=16 (i32.const 0) (local.get $name))
  (call $imp_fail (i32.const $E_HOST_FIELD) (local.get $pos))
  unreachable)

;; $cost charges a built-in's work: n/16 steps, and one more.
(func $cost (param $n i32) (param $pos i32)
  (global.set $steps (i32.sub (global.get $steps) (i32.add (i32.shr_u (local.get $n) (i32.const 4)) (i32.const 1))))
  (if (i32.lt_s (global.get $steps) (i32.const 0)) (then (call $fail_steps (local.get $pos)))))

;; ---- memory ----

;; $grow_to makes the memory reach end; 0 when it can't grow that far.
(func $grow_to (param $end i32) (result i32)
  (local $have i32)
  (local.set $have (i32.shl (memory.size) (i32.const 16)))
  (if (i32.le_u (local.get $end) (local.get $have)) (then (return (i32.const 1))))
  (i32.ne
    (memory.grow (i32.shr_u (i32.add (i32.sub (local.get $end) (local.get $have)) (i32.const 65535)) (i32.const 16)))
    (i32.const -1)))

;; $alloc takes size bytes (header included) off the heap, zeroed, counting them against the call's budget.
(func $alloc (param $size i32) (param $tag i32) (param $pos i32) (result i32)
  (local $p i32) (local $end i32)
  (local.set $size (i32.and (i32.add (local.get $size) (i32.const 7)) (i32.const -8)))
  (global.set $alloc (i32.add (global.get $alloc) (local.get $size)))
  (if (i32.gt_u (global.get $alloc) (i32.const $MAX_HEAP)) (then (call $fail_memory (local.get $pos))))
  (local.set $p (global.get $hp))
  (local.set $end (i32.add (local.get $p) (local.get $size)))
  (if (i32.eqz (call $grow_to (local.get $end))) (then (call $fail_memory (local.get $pos))))
  (global.set $hp (local.get $end))
  (i32.store (local.get $p) (local.get $tag))
  (i32.store offset=4 (local.get $p) (local.get $size))
  (local.get $p))

(func $store_new (param $cap i32) (param $pos i32) (result i32)
  (local $s i32)
  (local.set $s (call $alloc (i32.add (i32.const 16) (i32.shl (local.get $cap) (i32.const 3))) (i32.const $T_STORE) (local.get $pos)))
  (i32.store offset=12 (local.get $s) (local.get $cap))
  (local.get $s))

;; $store_grow gives a store room for n 8-byte slots, copying its first used ones; it returns the store to use.
(func $store_grow (param $s i32) (param $used i32) (param $n i32) (param $pos i32) (result i32)
  (local $cap i32) (local $ns i32)
  (local.set $cap (i32.load offset=12 (local.get $s)))
  (if (i32.le_u (local.get $n) (local.get $cap)) (then (return (local.get $s))))
  (local.set $cap (i32.shl (local.get $cap) (i32.const 1)))
  (if (i32.lt_u (local.get $cap) (local.get $n)) (then (local.set $cap (local.get $n))))
  (local.set $ns (call $store_new (local.get $cap) (local.get $pos)))
  (memory.copy (i32.add (local.get $ns) (i32.const 16)) (i32.add (local.get $s) (i32.const 16)) (i32.shl (local.get $used) (i32.const 3)))
  (local.get $ns))

;; ---- strings ----

(func $str_new (param $len i32) (param $pos i32) (result i32)
  (local $p i32)
  (local.set $p (call $alloc (i32.add (local.get $len) (i32.const 16)) (i32.const $T_STRING) (local.get $pos)))
  (i32.store offset=12 (local.get $p) (local.get $len))
  (local.get $p))

(func $str_check (param $len i32) (param $pos i32)
  (if (i32.gt_u (local.get $len) (i32.const $MAX_STRING)) (then (call $fail_string (local.get $pos)))))

(func $str_concat (param $a i32) (param $b i32) (param $pos i32) (result i32)
  (local $la i32) (local $lb i32) (local $p i32)
  (local.set $la (i32.load offset=12 (local.get $a)))
  (local.set $lb (i32.load offset=12 (local.get $b)))
  (call $str_check (i32.add (local.get $la) (local.get $lb)) (local.get $pos))
  (if (i32.eqz (local.get $la)) (then (return (local.get $b))))
  (if (i32.eqz (local.get $lb)) (then (return (local.get $a))))
  (local.set $p (call $str_new (i32.add (local.get $la) (local.get $lb)) (local.get $pos)))
  (memory.copy (i32.add (local.get $p) (i32.const 16)) (i32.add (local.get $a) (i32.const 16)) (local.get $la))
  (memory.copy (i32.add (i32.add (local.get $p) (i32.const 16)) (local.get $la)) (i32.add (local.get $b) (i32.const 16)) (local.get $lb))
  (local.get $p))

;; $bytes_eq compares n bytes at a and b.
(func $bytes_eq (param $a i32) (param $b i32) (param $n i32) (result i32)
  (local $i i32)
  (block $words (loop $next
    (br_if $words (i32.gt_u (i32.add (local.get $i) (i32.const 8)) (local.get $n)))
    (if (i64.ne (i64.load (i32.add (local.get $a) (local.get $i))) (i64.load (i32.add (local.get $b) (local.get $i))))
      (then (return (i32.const 0))))
    (local.set $i (i32.add (local.get $i) (i32.const 8)))
    (br $next)))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (if (i32.ne (i32.load8_u (i32.add (local.get $a) (local.get $i))) (i32.load8_u (i32.add (local.get $b) (local.get $i))))
      (then (return (i32.const 0))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (i32.const 1))

(func $str_eq (param $a i32) (param $b i32) (result i32)
  (local $n i32)
  (if (i32.eq (local.get $a) (local.get $b)) (then (return (i32.const 1))))
  (local.set $n (i32.load offset=12 (local.get $a)))
  (if (i32.ne (local.get $n) (i32.load offset=12 (local.get $b))) (then (return (i32.const 0))))
  (call $bytes_eq (i32.add (local.get $a) (i32.const 16)) (i32.add (local.get $b) (i32.const 16)) (local.get $n)))

;; $str_cmp compares byte by byte: -1, 0 or 1.
(func $str_cmp (param $a i32) (param $b i32) (result i32)
  (local $la i32) (local $lb i32) (local $n i32) (local $i i32) (local $x i32) (local $y i32)
  (local.set $la (i32.load offset=12 (local.get $a)))
  (local.set $lb (i32.load offset=12 (local.get $b)))
  (local.set $n (select (local.get $la) (local.get $lb) (i32.lt_u (local.get $la) (local.get $lb))))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (local.set $x (i32.load8_u offset=16 (i32.add (local.get $a) (local.get $i))))
    (local.set $y (i32.load8_u offset=16 (i32.add (local.get $b) (local.get $i))))
    (if (i32.ne (local.get $x) (local.get $y))
      (then (return (select (i32.const -1) (i32.const 1) (i32.lt_u (local.get $x) (local.get $y))))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (if (i32.lt_u (local.get $la) (local.get $lb)) (then (return (i32.const -1))))
  (i32.gt_u (local.get $la) (local.get $lb)))

;; $num_str writes a number in its shortest form. Whole numbers below 2^53 are written here; the host writes the
;; rest the way Go's strconv.FormatFloat(x, 'f', -1, 64) does.
(func $num_str (param $x f64) (param $pos i32) (result i32)
  (local $n i64) (local $neg i32) (local $i i32) (local $len i32) (local $p i32)
  (if (i32.and (f64.eq (f64.trunc (local.get $x)) (local.get $x))
               (f64.lt (f64.abs (local.get $x)) (f64.const 9007199254740992)))
    (then
      (if (f64.eq (local.get $x) (f64.const 0))
        (then
          (if (i64.lt_s (i64.reinterpret_f64 (local.get $x)) (i64.const 0)) (then (return (i32.const $S_negzero))))
          (return (i32.const $S_zero))))
      (local.set $n (i64.trunc_f64_s (local.get $x)))
      (local.set $neg (i64.lt_s (local.get $n) (i64.const 0)))
      (if (local.get $neg) (then (local.set $n (i64.sub (i64.const 0) (local.get $n)))))
      ;; the digits go into the scratch slots from the end
      (local.set $i (i32.const 48))
      (loop $next
        (local.set $i (i32.sub (local.get $i) (i32.const 1)))
        (i32.store8 (local.get $i) (i32.add (i32.const 48) (i32.wrap_i64 (i64.rem_u (local.get $n) (i64.const 10)))))
        (local.set $n (i64.div_u (local.get $n) (i64.const 10)))
        (br_if $next (i64.ne (local.get $n) (i64.const 0))))
      (if (local.get $neg)
        (then
          (local.set $i (i32.sub (local.get $i) (i32.const 1)))
          (i32.store8 (local.get $i) (i32.const 45))))
      (local.set $len (i32.sub (i32.const 48) (local.get $i)))
      (local.set $p (call $str_new (local.get $len) (local.get $pos)))
      (memory.copy (i32.add (local.get $p) (i32.const 16)) (local.get $i) (local.get $len))
      (return (local.get $p))))
  (global.set $npos (local.get $pos))
  (call $imp_fmt_num (local.get $x)))

;; $utf8_dec decodes the rune at p (before end) as Go's utf8.DecodeRune does: (width << 24) | rune, and
;; U+FFFD with width 1 for an invalid byte.
(func $utf8_dec (param $p i32) (param $end i32) (result i32)
  (local $c i32) (local $c1 i32) (local $c2 i32) (local $c3 i32) (local $n i32) (local $lo i32) (local $hi i32)
  (local.set $c (i32.load8_u (local.get $p)))
  (if (i32.lt_u (local.get $c) (i32.const 0x80)) (then (return (i32.or (local.get $c) (i32.const 0x1000000)))))
  (local.set $n (i32.sub (local.get $end) (local.get $p)))
  (local.set $lo (i32.const 0x80))
  (local.set $hi (i32.const 0xbf))
  (if (i32.and (i32.ge_u (local.get $c) (i32.const 0xc2)) (i32.le_u (local.get $c) (i32.const 0xdf)))
    (then
      (if (i32.lt_u (local.get $n) (i32.const 2)) (then (return (i32.const 0x100fffd))))
      (local.set $c1 (i32.load8_u offset=1 (local.get $p)))
      (if (i32.or (i32.lt_u (local.get $c1) (i32.const 0x80)) (i32.gt_u (local.get $c1) (i32.const 0xbf)))
        (then (return (i32.const 0x100fffd))))
      (return (i32.or (i32.const 0x2000000)
        (i32.or (i32.shl (i32.and (local.get $c) (i32.const 0x1f)) (i32.const 6)) (i32.and (local.get $c1) (i32.const 0x3f)))))))
  (if (i32.and (i32.ge_u (local.get $c) (i32.const 0xe0)) (i32.le_u (local.get $c) (i32.const 0xef)))
    (then
      (if (i32.lt_u (local.get $n) (i32.const 3)) (then (return (i32.const 0x100fffd))))
      (if (i32.eq (local.get $c) (i32.const 0xe0)) (then (local.set $lo (i32.const 0xa0))))
      (if (i32.eq (local.get $c) (i32.const 0xed)) (then (local.set $hi (i32.const 0x9f))))
      (local.set $c1 (i32.load8_u offset=1 (local.get $p)))
      (local.set $c2 (i32.load8_u offset=2 (local.get $p)))
      (if (i32.or (i32.or (i32.lt_u (local.get $c1) (local.get $lo)) (i32.gt_u (local.get $c1) (local.get $hi)))
                  (i32.or (i32.lt_u (local.get $c2) (i32.const 0x80)) (i32.gt_u (local.get $c2) (i32.const 0xbf))))
        (then (return (i32.const 0x100fffd))))
      (return (i32.or (i32.const 0x3000000)
        (i32.or (i32.shl (i32.and (local.get $c) (i32.const 0x0f)) (i32.const 12))
          (i32.or (i32.shl (i32.and (local.get $c1) (i32.const 0x3f)) (i32.const 6)) (i32.and (local.get $c2) (i32.const 0x3f))))))))
  (if (i32.and (i32.ge_u (local.get $c) (i32.const 0xf0)) (i32.le_u (local.get $c) (i32.const 0xf4)))
    (then
      (if (i32.lt_u (local.get $n) (i32.const 4)) (then (return (i32.const 0x100fffd))))
      (if (i32.eq (local.get $c) (i32.const 0xf0)) (then (local.set $lo (i32.const 0x90))))
      (if (i32.eq (local.get $c) (i32.const 0xf4)) (then (local.set $hi (i32.const 0x8f))))
      (local.set $c1 (i32.load8_u offset=1 (local.get $p)))
      (local.set $c2 (i32.load8_u offset=2 (local.get $p)))
      (local.set $c3 (i32.load8_u offset=3 (local.get $p)))
      (if (i32.or (i32.or (i32.lt_u (local.get $c1) (local.get $lo)) (i32.gt_u (local.get $c1) (local.get $hi)))
                  (i32.or (i32.or (i32.lt_u (local.get $c2) (i32.const 0x80)) (i32.gt_u (local.get $c2) (i32.const 0xbf)))
                          (i32.or (i32.lt_u (local.get $c3) (i32.const 0x80)) (i32.gt_u (local.get $c3) (i32.const 0xbf)))))
        (then (return (i32.const 0x100fffd))))
      (return (i32.or (i32.const 0x4000000)
        (i32.or (i32.shl (i32.and (local.get $c) (i32.const 0x07)) (i32.const 18))
          (i32.or (i32.shl (i32.and (local.get $c1) (i32.const 0x3f)) (i32.const 12))
            (i32.or (i32.shl (i32.and (local.get $c2) (i32.const 0x3f)) (i32.const 6)) (i32.and (local.get $c3) (i32.const 0x3f)))))))))
  (i32.const 0x100fffd))

;; $utf8_dec_last decodes the last rune before end (after start), as Go's utf8.DecodeLastRune does.
(func $utf8_dec_last (param $start i32) (param $end i32) (result i32)
  (local $s i32) (local $lim i32) (local $d i32) (local $c i32)
  (local.set $s (i32.sub (local.get $end) (i32.const 1)))
  (local.set $c (i32.load8_u (local.get $s)))
  (if (i32.lt_u (local.get $c) (i32.const 0x80)) (then (return (i32.or (local.get $c) (i32.const 0x1000000)))))
  (local.set $lim (i32.sub (local.get $end) (i32.const 4)))
  (if (i32.lt_s (local.get $lim) (local.get $start)) (then (local.set $lim (local.get $start))))
  (block $found (loop $back
    (local.set $s (i32.sub (local.get $s) (i32.const 1)))
    (br_if $found (i32.lt_s (local.get $s) (local.get $lim)))
    (br_if $found (i32.ne (i32.and (i32.load8_u (local.get $s)) (i32.const 0xc0)) (i32.const 0x80)))
    (br $back)))
  (if (i32.lt_s (local.get $s) (local.get $start)) (then (local.set $s (local.get $start))))
  (local.set $d (call $utf8_dec (local.get $s) (local.get $end)))
  (if (i32.ne (i32.add (local.get $s) (i32.shr_u (local.get $d) (i32.const 24))) (local.get $end))
    (then (return (i32.const 0x100fffd))))
  (local.get $d))

;; $rune_len is how many bytes $utf8_enc writes for r.
(func $rune_len (param $r i32) (result i32)
  (if (i32.lt_u (local.get $r) (i32.const 0x80)) (then (return (i32.const 1))))
  (if (i32.lt_u (local.get $r) (i32.const 0x800)) (then (return (i32.const 2))))
  (if (i32.or (i32.gt_u (local.get $r) (i32.const 0x10ffff)) (i32.eq (i32.and (local.get $r) (i32.const 0xfffff800)) (i32.const 0xd800)))
    (then (return (i32.const 3))))
  (if (i32.lt_u (local.get $r) (i32.const 0x10000)) (then (return (i32.const 3))))
  (i32.const 4))

;; $utf8_enc writes r at d as Go's utf8.EncodeRune does (U+FFFD for surrogates and runes past U+10FFFF), and
;; returns its width.
(func $utf8_enc (param $d i32) (param $r i32) (result i32)
  (if (i32.lt_u (local.get $r) (i32.const 0x80))
    (then (i32.store8 (local.get $d) (local.get $r)) (return (i32.const 1))))
  (if (i32.lt_u (local.get $r) (i32.const 0x800))
    (then
      (i32.store8 (local.get $d) (i32.or (i32.const 0xc0) (i32.shr_u (local.get $r) (i32.const 6))))
      (i32.store8 offset=1 (local.get $d) (i32.or (i32.const 0x80) (i32.and (local.get $r) (i32.const 0x3f))))
      (return (i32.const 2))))
  (if (i32.or (i32.gt_u (local.get $r) (i32.const 0x10ffff)) (i32.eq (i32.and (local.get $r) (i32.const 0xfffff800)) (i32.const 0xd800)))
    (then (local.set $r (i32.const 0xfffd))))
  (if (i32.lt_u (local.get $r) (i32.const 0x10000))
    (then
      (i32.store8 (local.get $d) (i32.or (i32.const 0xe0) (i32.shr_u (local.get $r) (i32.const 12))))
      (i32.store8 offset=1 (local.get $d) (i32.or (i32.const 0x80) (i32.and (i32.shr_u (local.get $r) (i32.const 6)) (i32.const 0x3f))))
      (i32.store8 offset=2 (local.get $d) (i32.or (i32.const 0x80) (i32.and (local.get $r) (i32.const 0x3f))))
      (return (i32.const 3))))
  (i32.store8 (local.get $d) (i32.or (i32.const 0xf0) (i32.shr_u (local.get $r) (i32.const 18))))
  (i32.store8 offset=1 (local.get $d) (i32.or (i32.const 0x80) (i32.and (i32.shr_u (local.get $r) (i32.const 12)) (i32.const 0x3f))))
  (i32.store8 offset=2 (local.get $d) (i32.or (i32.const 0x80) (i32.and (i32.shr_u (local.get $r) (i32.const 6)) (i32.const 0x3f))))
  (i32.store8 offset=3 (local.get $d) (i32.or (i32.const 0x80) (i32.and (local.get $r) (i32.const 0x3f))))
  (i32.const 4))

;; $str_runes counts a string's runes, an invalid byte as one, as Go's utf8.RuneCountInString does.
(func $str_runes (param $s i32) (result i32)
  (call $runes_in (i32.add (local.get $s) (i32.const 16)) (i32.add (i32.add (local.get $s) (i32.const 16)) (i32.load offset=12 (local.get $s)))))

(func $runes_in (param $p i32) (param $end i32) (result i32)
  (local $n i32)
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $p) (local.get $end)))
    (if (i32.lt_u (i32.load8_u (local.get $p)) (i32.const 0x80))
      (then (local.set $p (i32.add (local.get $p) (i32.const 1))))
      (else (local.set $p (i32.add (local.get $p) (i32.shr_u (call $utf8_dec (local.get $p) (local.get $end)) (i32.const 24))))))
    (local.set $n (i32.add (local.get $n) (i32.const 1)))
    (br $next)))
  (local.get $n))

;; $slice_bound turns one of slice's arguments into a position within 0..n, as TypeScript's slice does (negative
;; ones count from the end); def when the argument wasn't given.
(func $slice_bound (param $x f64) (param $given i32) (param $def i32) (param $n i32) (param $pos i32) (result i32)
  (local $f f64) (local $i i32)
  (if (i32.eqz (local.get $given)) (then (return (local.get $def))))
  (if (f64.ne (f64.trunc (local.get $x)) (local.get $x)) (then (call $fail_slice (local.get $x) (local.get $pos))))
  (local.set $f (f64.max (f64.min (local.get $x) (f64.convert_i32_s (local.get $n))) (f64.neg (f64.convert_i32_s (local.get $n)))))
  (local.set $i (i32.trunc_f64_s (local.get $f)))
  (if (i32.lt_s (local.get $i) (i32.const 0)) (then (local.set $i (i32.add (local.get $i) (local.get $n)))))
  (local.get $i))

;; $str_slice is s.slice(a, b) over runes, with nargs of them given; the runes are written again, so an invalid
;; byte becomes U+FFFD, as Go's []rune conversion makes it.
(func $str_slice (param $s i32) (param $a f64) (param $b f64) (param $nargs i32) (param $pos i32) (result i32)
  (local $n i32) (local $start i32) (local $end i32) (local $p i32) (local $stop i32) (local $i i32) (local $d i32)
  (local $len i32) (local $out i32) (local $w i32)
  (local.set $p (i32.add (local.get $s) (i32.const 16)))
  (local.set $stop (i32.add (local.get $p) (i32.load offset=12 (local.get $s))))
  (local.set $n (call $runes_in (local.get $p) (local.get $stop)))
  (local.set $start (call $slice_bound (local.get $a) (i32.ge_u (local.get $nargs) (i32.const 1)) (i32.const 0) (local.get $n) (local.get $pos)))
  (local.set $end (call $slice_bound (local.get $b) (i32.ge_u (local.get $nargs) (i32.const 2)) (local.get $n) (local.get $n) (local.get $pos)))
  (if (i32.lt_s (local.get $end) (local.get $start)) (then (local.set $end (local.get $start))))
  ;; how long it is
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $p) (local.get $stop)))
    (br_if $done (i32.ge_s (local.get $i) (local.get $end)))
    (local.set $d (call $utf8_dec (local.get $p) (local.get $stop)))
    (if (i32.ge_s (local.get $i) (local.get $start))
      (then (local.set $len (i32.add (local.get $len) (call $rune_len (i32.and (local.get $d) (i32.const 0xffffff)))))))
    (local.set $p (i32.add (local.get $p) (i32.shr_u (local.get $d) (i32.const 24))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (call $str_check (local.get $len) (local.get $pos))
  (local.set $out (call $str_new (local.get $len) (local.get $pos)))
  (local.set $w (i32.add (local.get $out) (i32.const 16)))
  (local.set $p (i32.add (local.get $s) (i32.const 16)))
  (local.set $i (i32.const 0))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $p) (local.get $stop)))
    (br_if $done (i32.ge_s (local.get $i) (local.get $end)))
    (local.set $d (call $utf8_dec (local.get $p) (local.get $stop)))
    (if (i32.ge_s (local.get $i) (local.get $start))
      (then (local.set $w (i32.add (local.get $w) (call $utf8_enc (local.get $w) (i32.and (local.get $d) (i32.const 0xffffff)))))))
    (local.set $p (i32.add (local.get $p) (i32.shr_u (local.get $d) (i32.const 24))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (local.get $out))

;; $str_find finds sub's bytes in s: their byte offset, or -1. An empty sub is at 0.
(func $str_find (param $s i32) (param $sub i32) (result i32)
  (local $n i32) (local $m i32) (local $i i32) (local $first i32)
  (local.set $n (i32.load offset=12 (local.get $s)))
  (local.set $m (i32.load offset=12 (local.get $sub)))
  (if (i32.eqz (local.get $m)) (then (return (i32.const 0))))
  (if (i32.gt_u (local.get $m) (local.get $n)) (then (return (i32.const -1))))
  (local.set $first (i32.load8_u offset=16 (local.get $sub)))
  (block $done (loop $next
    (br_if $done (i32.gt_u (i32.add (local.get $i) (local.get $m)) (local.get $n)))
    (if (i32.eq (i32.load8_u offset=16 (i32.add (local.get $s) (local.get $i))) (local.get $first))
      (then
        (if (call $bytes_eq (i32.add (i32.add (local.get $s) (i32.const 16)) (local.get $i)) (i32.add (local.get $sub) (i32.const 16)) (local.get $m))
          (then (return (local.get $i))))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (i32.const -1))

;; $str_indexof is s.indexOf(sub): where sub starts, in runes, or -1.
(func $str_indexof (param $s i32) (param $sub i32) (result f64)
  (local $i i32)
  (local.set $i (call $str_find (local.get $s) (local.get $sub)))
  (if (i32.lt_s (local.get $i) (i32.const 0)) (then (return (f64.const -1))))
  (f64.convert_i32_u (call $runes_in (i32.add (local.get $s) (i32.const 16)) (i32.add (i32.add (local.get $s) (i32.const 16)) (local.get $i)))))

(func $str_starts (param $s i32) (param $p i32) (result i32)
  (local $m i32)
  (local.set $m (i32.load offset=12 (local.get $p)))
  (if (i32.gt_u (local.get $m) (i32.load offset=12 (local.get $s))) (then (return (i32.const 0))))
  (call $bytes_eq (i32.add (local.get $s) (i32.const 16)) (i32.add (local.get $p) (i32.const 16)) (local.get $m)))

(func $str_ends (param $s i32) (param $p i32) (result i32)
  (local $m i32) (local $n i32)
  (local.set $m (i32.load offset=12 (local.get $p)))
  (local.set $n (i32.load offset=12 (local.get $s)))
  (if (i32.gt_u (local.get $m) (local.get $n)) (then (return (i32.const 0))))
  (call $bytes_eq (i32.add (i32.add (local.get $s) (i32.const 16)) (i32.sub (local.get $n) (local.get $m))) (i32.add (local.get $p) (i32.const 16)) (local.get $m)))

;; $case_rune maps r to upper case (lower 0) or lower case (lower 1), as Go's unicode.ToUpper and ToLower do,
;; through the case ranges table ($CASES: lo, hi, upper delta, lower delta; a delta of $UPPER_LOWER alternates).
(func $case_rune (param $r i32) (param $lower i32) (result i32)
  (local $lo i32) (local $hi i32) (local $m i32) (local $e i32) (local $d i32)
  (if (i32.lt_u (local.get $r) (i32.const 0x80))
    (then
      (if (local.get $lower)
        (then (if (i32.and (i32.ge_u (local.get $r) (i32.const 65)) (i32.le_u (local.get $r) (i32.const 90)))
          (then (return (i32.add (local.get $r) (i32.const 32))))))
        (else (if (i32.and (i32.ge_u (local.get $r) (i32.const 97)) (i32.le_u (local.get $r) (i32.const 122)))
          (then (return (i32.sub (local.get $r) (i32.const 32)))))))
      (return (local.get $r))))
  (local.set $hi (i32.const $NCASES))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $lo) (local.get $hi)))
    (local.set $m (i32.shr_u (i32.add (local.get $lo) (local.get $hi)) (i32.const 1)))
    (local.set $e (i32.add (i32.const $CASES) (i32.shl (local.get $m) (i32.const 4))))
    (if (i32.lt_u (local.get $r) (i32.load (local.get $e)))
      (then (local.set $hi (local.get $m)))
      (else
        (if (i32.gt_u (local.get $r) (i32.load offset=4 (local.get $e)))
          (then (local.set $lo (i32.add (local.get $m) (i32.const 1))))
          (else
            (local.set $d (i32.load offset=8 (i32.add (local.get $e) (i32.shl (local.get $lower) (i32.const 2)))))
            (if (i32.eq (local.get $d) (i32.const $UPPER_LOWER))
              (then (return (i32.add (i32.load (local.get $e))
                (i32.or (i32.and (i32.sub (local.get $r) (i32.load (local.get $e))) (i32.const -2)) (local.get $lower))))))
            (return (i32.add (local.get $r) (local.get $d)))))))
    (br $next)))
  (local.get $r))

;; $str_case is toUpperCase (lower 0) or toLowerCase (lower 1), as Go's strings.ToUpper and ToLower do: every
;; rune mapped, an invalid byte becoming U+FFFD.
(func $str_case (param $s i32) (param $lower i32) (param $pos i32) (result i32)
  (local $p i32) (local $stop i32) (local $d i32) (local $r i32) (local $m i32) (local $len i32) (local $same i32)
  (local $out i32) (local $w i32)
  (local.set $p (i32.add (local.get $s) (i32.const 16)))
  (local.set $stop (i32.add (local.get $p) (i32.load offset=12 (local.get $s))))
  (local.set $same (i32.const 1))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $p) (local.get $stop)))
    (local.set $d (call $utf8_dec (local.get $p) (local.get $stop)))
    (local.set $r (i32.and (local.get $d) (i32.const 0xffffff)))
    (local.set $m (call $case_rune (local.get $r) (local.get $lower)))
    (if (i32.or (i32.ne (local.get $m) (local.get $r)) (i32.eq (local.get $d) (i32.const 0x100fffd)))
      (then (local.set $same (i32.const 0))))
    (local.set $len (i32.add (local.get $len) (call $rune_len (local.get $m))))
    (local.set $p (i32.add (local.get $p) (i32.shr_u (local.get $d) (i32.const 24))))
    (br $next)))
  (if (local.get $same) (then (return (local.get $s))))
  (call $str_check (local.get $len) (local.get $pos))
  (local.set $out (call $str_new (local.get $len) (local.get $pos)))
  (local.set $w (i32.add (local.get $out) (i32.const 16)))
  (local.set $p (i32.add (local.get $s) (i32.const 16)))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $p) (local.get $stop)))
    (local.set $d (call $utf8_dec (local.get $p) (local.get $stop)))
    (local.set $w (i32.add (local.get $w) (call $utf8_enc (local.get $w) (call $case_rune (i32.and (local.get $d) (i32.const 0xffffff)) (local.get $lower)))))
    (local.set $p (i32.add (local.get $p) (i32.shr_u (local.get $d) (i32.const 24))))
    (br $next)))
  (local.get $out))

;; $is_space is Go's unicode.IsSpace.
(func $is_space (param $r i32) (result i32)
  (if (i32.le_u (local.get $r) (i32.const 0xff))
    (then (return (i32.or
      (i32.or (i32.and (i32.ge_u (local.get $r) (i32.const 9)) (i32.le_u (local.get $r) (i32.const 13))) (i32.eq (local.get $r) (i32.const 32)))
      (i32.or (i32.eq (local.get $r) (i32.const 0x85)) (i32.eq (local.get $r) (i32.const 0xa0)))))))
  (i32.or
    (i32.or (i32.eq (local.get $r) (i32.const 0x1680)) (i32.and (i32.ge_u (local.get $r) (i32.const 0x2000)) (i32.le_u (local.get $r) (i32.const 0x200a))))
    (i32.or
      (i32.or (i32.eq (local.get $r) (i32.const 0x2028)) (i32.eq (local.get $r) (i32.const 0x2029)))
      (i32.or (i32.or (i32.eq (local.get $r) (i32.const 0x202f)) (i32.eq (local.get $r) (i32.const 0x205f))) (i32.eq (local.get $r) (i32.const 0x3000))))))

;; $substr is the bytes from..to of s (offsets into its text), s itself when that is all of it.
(func $substr (param $s i32) (param $from i32) (param $to i32) (param $pos i32) (result i32)
  (local $p i32)
  (if (i32.and (i32.eqz (local.get $from)) (i32.eq (local.get $to) (i32.load offset=12 (local.get $s))))
    (then (return (local.get $s))))
  (if (i32.eq (local.get $from) (local.get $to)) (then (return (i32.const $S_empty))))
  (local.set $p (call $str_new (i32.sub (local.get $to) (local.get $from)) (local.get $pos)))
  (memory.copy (i32.add (local.get $p) (i32.const 16)) (i32.add (i32.add (local.get $s) (i32.const 16)) (local.get $from)) (i32.sub (local.get $to) (local.get $from)))
  (local.get $p))

;; $str_trim is Go's strings.TrimSpace.
(func $str_trim (param $s i32) (param $pos i32) (result i32)
  (local $base i32) (local $a i32) (local $b i32) (local $d i32)
  (local.set $base (i32.add (local.get $s) (i32.const 16)))
  (local.set $a (local.get $base))
  (local.set $b (i32.add (local.get $base) (i32.load offset=12 (local.get $s))))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $a) (local.get $b)))
    (local.set $d (call $utf8_dec (local.get $a) (local.get $b)))
    (br_if $done (i32.eqz (call $is_space (i32.and (local.get $d) (i32.const 0xffffff)))))
    (local.set $a (i32.add (local.get $a) (i32.shr_u (local.get $d) (i32.const 24))))
    (br $next)))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $a) (local.get $b)))
    (local.set $d (call $utf8_dec_last (local.get $a) (local.get $b)))
    (br_if $done (i32.eqz (call $is_space (i32.and (local.get $d) (i32.const 0xffffff)))))
    (local.set $b (i32.sub (local.get $b) (i32.shr_u (local.get $d) (i32.const 24))))
    (br $next)))
  (call $substr (local.get $s) (i32.sub (local.get $a) (local.get $base)) (i32.sub (local.get $b) (local.get $base)) (local.get $pos)))

;; $str_split is Go's strings.Split: on every sep, or, with an empty sep, into its UTF-8 sequences (an invalid
;; byte on its own).
(func $str_split (param $s i32) (param $sep i32) (param $pos i32) (result i32)
  (local $n i32) (local $m i32) (local $parts i32) (local $i i32) (local $start i32) (local $arr i32) (local $k i32)
  (local $base i32) (local $d i32) (local $w i32)
  (local.set $n (i32.load offset=12 (local.get $s)))
  (local.set $m (i32.load offset=12 (local.get $sep)))
  (local.set $base (i32.add (local.get $s) (i32.const 16)))
  (if (i32.eqz (local.get $m))
    (then
      (local.set $parts (call $str_runes (local.get $s)))
      (if (i32.gt_u (local.get $parts) (i32.const $MAX_LENGTH)) (then (call $fail_length (i32.const $S_array) (local.get $pos))))
      (local.set $arr (call $arr_new (i32.const $T_ARR_REFS) (local.get $parts) (local.get $pos)))
      (block $done (loop $next
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $d (call $utf8_dec (i32.add (local.get $base) (local.get $i)) (i32.add (local.get $base) (local.get $n))))
        (local.set $w (i32.shr_u (local.get $d) (i32.const 24)))
        (i32.store offset=16 (i32.add (i32.load offset=16 (local.get $arr)) (i32.shl (local.get $k) (i32.const 3)))
          (call $substr (local.get $s) (local.get $i) (i32.add (local.get $i) (local.get $w)) (local.get $pos)))
        (local.set $i (i32.add (local.get $i) (local.get $w)))
        (local.set $k (i32.add (local.get $k) (i32.const 1)))
        (br $next)))
      (return (local.get $arr))))
  ;; count the parts
  (local.set $parts (i32.const 1))
  (block $done (loop $next
    (br_if $done (i32.gt_u (i32.add (local.get $i) (local.get $m)) (local.get $n)))
    (if (call $bytes_eq (i32.add (local.get $base) (local.get $i)) (i32.add (local.get $sep) (i32.const 16)) (local.get $m))
      (then
        (local.set $parts (i32.add (local.get $parts) (i32.const 1)))
        (local.set $i (i32.add (local.get $i) (local.get $m))))
      (else (local.set $i (i32.add (local.get $i) (i32.const 1)))))
    (br $next)))
  (if (i32.gt_u (local.get $parts) (i32.const $MAX_LENGTH)) (then (call $fail_length (i32.const $S_array) (local.get $pos))))
  (local.set $arr (call $arr_new (i32.const $T_ARR_REFS) (local.get $parts) (local.get $pos)))
  (local.set $i (i32.const 0))
  (block $done (loop $next
    (br_if $done (i32.gt_u (i32.add (local.get $i) (local.get $m)) (local.get $n)))
    (if (call $bytes_eq (i32.add (local.get $base) (local.get $i)) (i32.add (local.get $sep) (i32.const 16)) (local.get $m))
      (then
        (i32.store offset=16 (i32.add (i32.load offset=16 (local.get $arr)) (i32.shl (local.get $k) (i32.const 3)))
          (call $substr (local.get $s) (local.get $start) (local.get $i) (local.get $pos)))
        (local.set $k (i32.add (local.get $k) (i32.const 1)))
        (local.set $i (i32.add (local.get $i) (local.get $m)))
        (local.set $start (local.get $i)))
      (else (local.set $i (i32.add (local.get $i) (i32.const 1)))))
    (br $next)))
  (i32.store offset=16 (i32.add (i32.load offset=16 (local.get $arr)) (i32.shl (local.get $k) (i32.const 3)))
    (call $substr (local.get $s) (local.get $start) (local.get $n) (local.get $pos)))
  (local.get $arr))

;; $num_parse reads a number from a string, spaces around it allowed, as Go's strconv.ParseFloat does (the host
;; parses); name is the function, for the error.
(func $num_parse (param $s i32) (param $name i32) (param $pos i32) (result f64)
  (local $t i32)
  (local.set $t (call $str_trim (local.get $s) (local.get $pos)))
  (if (i32.eqz (call $imp_parse_num (i32.add (local.get $t) (i32.const 16)) (i32.load offset=12 (local.get $t))))
    (then (call $fail_notnum (local.get $name) (local.get $s) (local.get $pos))))
  (f64.load offset=16 (i32.const 0)))

(func $bool_parse (param $s i32) (param $pos i32) (result i32)
  (if (call $str_eq (local.get $s) (i32.const $S_true)) (then (return (i32.const 1))))
  (if (call $str_eq (local.get $s) (i32.const $S_false)) (then (return (i32.const 0))))
  (call $fail_notbool (i32.const $S_bool) (local.get $s) (local.get $pos))
  (i32.const 0))

;; ---- numbers ----

;; $mod is %, which keeps the sign of a as Go's math.Mod does; whole numbers are worked out here, the rest by
;; the host.
(func $mod (param $a f64) (param $b f64) (param $pos i32) (result f64)
  (if (f64.eq (local.get $b) (f64.const 0)) (then (call $fail_divzero (local.get $pos))))
  (if (i32.and
        (i32.and (f64.eq (f64.trunc (local.get $a)) (local.get $a)) (f64.eq (f64.trunc (local.get $b)) (local.get $b)))
        (i32.and (f64.lt (f64.abs (local.get $a)) (f64.const 9007199254740992)) (f64.lt (f64.abs (local.get $b)) (f64.const 9007199254740992))))
    (then (return (f64.copysign
      (f64.convert_i64_s (i64.rem_s (i64.trunc_f64_s (local.get $a)) (i64.trunc_f64_s (local.get $b))))
      (local.get $a)))))
  (call $imp_fmod (local.get $a) (local.get $b)))

;; $round rounds halves away from zero, as Go's math.Round does.
(func $round (param $x f64) (result f64)
  (local $t f64)
  (local.set $t (f64.trunc (local.get $x)))
  (if (f64.ge (f64.abs (f64.sub (local.get $x) (local.get $t))) (f64.const 0.5))
    (then (local.set $t (f64.add (local.get $t) (f64.copysign (f64.const 1) (local.get $x))))))
  (local.get $t))

;; ---- arrays ----

(func $arr_new (param $tag i32) (param $n i32) (param $pos i32) (result i32)
  (local $a i32) (local $cap i32)
  (if (i32.gt_u (local.get $n) (i32.const $MAX_LENGTH)) (then (call $fail_length (i32.const $S_array) (local.get $pos))))
  (local.set $cap (select (local.get $n) (i32.const 4) (i32.gt_u (local.get $n) (i32.const 4))))
  (local.set $a (call $alloc (i32.const 24) (local.get $tag) (local.get $pos)))
  (i32.store offset=12 (local.get $a) (local.get $n))
  (i32.store offset=16 (local.get $a) (call $store_new (local.get $cap) (local.get $pos)))
  (local.get $a))

(func $arr_len (param $a i32) (result i32)
  (if (result i32) (local.get $a) (then (i32.load offset=12 (local.get $a))) (else (i32.const 0))))

;; $arr_reserve gives the array room for n elements in all.
(func $arr_reserve (param $a i32) (param $n i32) (param $pos i32)
  (if (i32.gt_u (local.get $n) (i32.const $MAX_LENGTH)) (then (call $fail_length (i32.const $S_array) (local.get $pos))))
  (i32.store offset=16 (local.get $a)
    (call $store_grow (i32.load offset=16 (local.get $a)) (i32.load offset=12 (local.get $a)) (local.get $n) (local.get $pos))))

;; $arr_elem is where element i of a is, failing when i isn't the index of one (a null array has none).
(func $arr_elem (param $a i32) (param $i f64) (param $pos i32) (result i32)
  (local $n i32)
  (local.set $n (call $arr_len (local.get $a)))
  (if (i32.or (f64.ne (f64.trunc (local.get $i)) (local.get $i))
        (i32.or (f64.lt (local.get $i) (f64.const 0)) (f64.ge (local.get $i) (f64.convert_i32_u (local.get $n)))))
    (then (call $fail_index (local.get $i) (local.get $n) (local.get $pos))))
  (i32.add (i32.add (i32.load offset=16 (local.get $a)) (i32.const 16)) (i32.shl (i32.trunc_f64_u (local.get $i)) (i32.const 3))))

;; $arr_elem_set is where element i of a is now, after the value to set it to was worked out.
(func $arr_elem_set (param $a i32) (param $i i32) (param $pos i32) (result i32)
  (local $n i32)
  (local.set $n (i32.load offset=12 (local.get $a)))
  (if (i32.ge_u (local.get $i) (local.get $n)) (then (call $fail_index_set (local.get $i) (local.get $n) (local.get $pos))))
  (i32.add (i32.add (i32.load offset=16 (local.get $a)) (i32.const 16)) (i32.shl (local.get $i) (i32.const 3))))

;; $arr_pop takes the last element off, returning where it was.
(func $arr_pop (param $a i32) (param $pos i32) (result i32)
  (local $n i32)
  (local.set $n (i32.load offset=12 (local.get $a)))
  (if (i32.eqz (local.get $n)) (then (call $fail_pop (local.get $pos))))
  (local.set $n (i32.sub (local.get $n) (i32.const 1)))
  (i32.store offset=12 (local.get $a) (local.get $n))
  (i32.add (i32.add (i32.load offset=16 (local.get $a)) (i32.const 16)) (i32.shl (local.get $n) (i32.const 3))))

;; $arr_slice is a.slice(x, y) with nargs of them given, a new array of tag.
(func $arr_slice (param $a i32) (param $x f64) (param $y f64) (param $nargs i32) (param $tag i32) (param $pos i32) (result i32)
  (local $n i32) (local $start i32) (local $end i32) (local $out i32)
  (local.set $n (call $arr_len (local.get $a)))
  (local.set $start (call $slice_bound (local.get $x) (i32.ge_u (local.get $nargs) (i32.const 1)) (i32.const 0) (local.get $n) (local.get $pos)))
  (local.set $end (call $slice_bound (local.get $y) (i32.ge_u (local.get $nargs) (i32.const 2)) (local.get $n) (local.get $n) (local.get $pos)))
  (if (i32.lt_s (local.get $end) (local.get $start)) (then (local.set $end (local.get $start))))
  (call $cost (i32.sub (local.get $end) (local.get $start)) (local.get $pos))
  (local.set $out (call $arr_new (local.get $tag) (i32.sub (local.get $end) (local.get $start)) (local.get $pos)))
  (if (i32.gt_s (local.get $end) (local.get $start))
    (then (memory.copy
      (i32.add (i32.load offset=16 (local.get $out)) (i32.const 16))
      (i32.add (i32.add (i32.load offset=16 (local.get $a)) (i32.const 16)) (i32.shl (local.get $start) (i32.const 3)))
      (i32.shl (i32.sub (local.get $end) (local.get $start)) (i32.const 3)))))
  (local.get $out))

;; $arr_find_f64, $arr_find_i32 and $arr_find_str are indexOf over numbers, over bools and references (by
;; identity), and over strings: the first index, or -1.
(func $arr_find_f64 (param $a i32) (param $x f64) (param $pos i32) (result f64)
  (local $n i32) (local $i i32) (local $p i32)
  (local.set $n (call $arr_len (local.get $a)))
  (call $cost (local.get $n) (local.get $pos))
  (if (i32.eqz (local.get $n)) (then (return (f64.const -1))))
  (local.set $p (i32.add (i32.load offset=16 (local.get $a)) (i32.const 16)))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (if (f64.eq (f64.load (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3)))) (local.get $x))
      (then (return (f64.convert_i32_u (local.get $i)))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (f64.const -1))

(func $arr_find_i32 (param $a i32) (param $x i32) (param $pos i32) (result f64)
  (local $n i32) (local $i i32) (local $p i32)
  (local.set $n (call $arr_len (local.get $a)))
  (call $cost (local.get $n) (local.get $pos))
  (if (i32.eqz (local.get $n)) (then (return (f64.const -1))))
  (local.set $p (i32.add (i32.load offset=16 (local.get $a)) (i32.const 16)))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (if (i32.eq (i32.load (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3)))) (local.get $x))
      (then (return (f64.convert_i32_u (local.get $i)))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (f64.const -1))

(func $arr_find_str (param $a i32) (param $x i32) (param $pos i32) (result f64)
  (local $n i32) (local $i i32) (local $p i32)
  (local.set $n (call $arr_len (local.get $a)))
  (call $cost (local.get $n) (local.get $pos))
  (if (i32.eqz (local.get $n)) (then (return (f64.const -1))))
  (local.set $p (i32.add (i32.load offset=16 (local.get $a)) (i32.const 16)))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (if (call $str_eq (i32.load (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3)))) (local.get $x))
      (then (return (f64.convert_i32_u (local.get $i)))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (f64.const -1))

;; $arr_join is a.join(sep) for elements of kind (0 numbers, 1 bools, 2 strings), in their text forms.
(func $arr_join (param $a i32) (param $sep i32) (param $kind i32) (param $pos i32) (result i32)
  (local $n i32) (local $i i32) (local $p i32) (local $parts i32) (local $part i32) (local $size i32)
  (local $seplen i32) (local $out i32) (local $w i32) (local $len i32)
  (local.set $n (call $arr_len (local.get $a)))
  (call $cost (local.get $n) (local.get $pos))
  (if (i32.eqz (local.get $n)) (then (return (i32.const $S_empty))))
  (local.set $seplen (i32.load offset=12 (local.get $sep)))
  (local.set $p (i32.add (i32.load offset=16 (local.get $a)) (i32.const 16)))
  (local.set $parts (call $store_new (local.get $n) (local.get $pos)))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (if (i32.eqz (local.get $kind))
      (then (local.set $part (call $num_str (f64.load (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3)))) (local.get $pos))))
      (else (if (i32.eq (local.get $kind) (i32.const 1))
        (then (local.set $part (select (i32.const $S_true) (i32.const $S_false) (i32.load (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3)))))))
        (else (local.set $part (i32.load (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3)))))))))
    (i32.store offset=16 (i32.add (local.get $parts) (i32.shl (local.get $i) (i32.const 3))) (local.get $part))
    (local.set $size (i32.add (local.get $size) (i32.add (i32.load offset=12 (local.get $part)) (local.get $seplen))))
    (call $str_check (local.get $size) (local.get $pos))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (local.set $len (i32.sub (local.get $size) (local.get $seplen)))
  (local.set $out (call $str_new (local.get $len) (local.get $pos)))
  (local.set $w (i32.add (local.get $out) (i32.const 16)))
  (local.set $i (i32.const 0))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (if (local.get $i)
      (then
        (memory.copy (local.get $w) (i32.add (local.get $sep) (i32.const 16)) (local.get $seplen))
        (local.set $w (i32.add (local.get $w) (local.get $seplen)))))
    (local.set $part (i32.load offset=16 (i32.add (local.get $parts) (i32.shl (local.get $i) (i32.const 3)))))
    (memory.copy (local.get $w) (i32.add (local.get $part) (i32.const 16)) (i32.load offset=12 (local.get $part)))
    (local.set $w (i32.add (local.get $w) (i32.load offset=12 (local.get $part))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (local.get $out))

;; ---- maps ----

(func $idx_new (param $cap i32) (param $pos i32) (result i32)
  (local $s i32)
  (local.set $s (call $alloc (i32.add (i32.const 16) (i32.shl (local.get $cap) (i32.const 2))) (i32.const $T_STORE) (local.get $pos)))
  (i32.store offset=12 (local.get $s) (local.get $cap))
  (local.get $s))

(func $map_new (param $flags i32) (param $pos i32) (result i32)
  (local $m i32)
  (local.set $m (call $alloc (i32.const 32) (i32.const $T_MAP) (local.get $pos)))
  (i32.store offset=16 (local.get $m) (call $store_new (i32.const 4) (local.get $pos)))
  (i32.store offset=20 (local.get $m) (call $store_new (i32.const 4) (local.get $pos)))
  (i32.store offset=24 (local.get $m) (call $idx_new (i32.const 8) (local.get $pos)))
  (i32.store offset=28 (local.get $m) (local.get $flags))
  (local.get $m))

(func $map_len (param $m i32) (result i32)
  (if (result i32) (local.get $m) (then (i32.load offset=12 (local.get $m))) (else (i32.const 0))))

(func $hash_str (param $s i32) (result i32)
  (local $h i32) (local $i i32) (local $n i32)
  (local.set $h (i32.const 0x811c9dc5))
  (local.set $n (i32.load offset=12 (local.get $s)))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (local.set $h (i32.mul (i32.xor (local.get $h) (i32.load8_u offset=16 (i32.add (local.get $s) (local.get $i)))) (i32.const 0x01000193)))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next)))
  (local.get $h))

(func $hash_num (param $x f64) (result i32)
  (local $b i64)
  (if (f64.eq (local.get $x) (f64.const 0)) (then (local.set $x (f64.const 0))))
  (local.set $b (i64.reinterpret_f64 (local.get $x)))
  (local.set $b (i64.mul (i64.xor (local.get $b) (i64.shr_u (local.get $b) (i64.const 29))) (i64.const 0xbf58476d1ce4e5b9)))
  (i32.wrap_i64 (i64.xor (local.get $b) (i64.shr_u (local.get $b) (i64.const 32)))))

;; $map_hash is the hash of entry i's key.
(func $map_hash (param $m i32) (param $i i32) (result i32)
  (local $k i32)
  (local.set $k (i32.add (i32.add (i32.load offset=16 (local.get $m)) (i32.const 16)) (i32.shl (local.get $i) (i32.const 3))))
  (if (result i32) (i32.and (i32.load offset=28 (local.get $m)) (i32.const 1))
    (then (call $hash_str (i32.load (local.get $k))))
    (else (call $hash_num (f64.load (local.get $k))))))

;; $map_index puts entry i into the index.
(func $map_index (param $m i32) (param $i i32)
  (local $idx i32) (local $mask i32) (local $h i32)
  (local.set $idx (i32.load offset=24 (local.get $m)))
  (local.set $mask (i32.sub (i32.load offset=12 (local.get $idx)) (i32.const 1)))
  (local.set $h (i32.and (call $map_hash (local.get $m) (local.get $i)) (local.get $mask)))
  (block $done (loop $next
    (br_if $done (i32.eqz (i32.load offset=16 (i32.add (local.get $idx) (i32.shl (local.get $h) (i32.const 2))))))
    (local.set $h (i32.and (i32.add (local.get $h) (i32.const 1)) (local.get $mask)))
    (br $next)))
  (i32.store offset=16 (i32.add (local.get $idx) (i32.shl (local.get $h) (i32.const 2))) (i32.add (local.get $i) (i32.const 1))))

;; $map_reindex makes a new index of cap slots for every entry.
(func $map_reindex (param $m i32) (param $cap i32) (param $pos i32)
  (local $i i32) (local $n i32)
  (i32.store offset=24 (local.get $m) (call $idx_new (local.get $cap) (local.get $pos)))
  (local.set $n (i32.load offset=12 (local.get $m)))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (call $map_index (local.get $m) (local.get $i))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next))))

(func $map_find_str (param $m i32) (param $k i32) (result i32)
  (local $idx i32) (local $mask i32) (local $h i32) (local $e i32)
  (if (i32.eqz (local.get $m)) (then (return (i32.const -1))))
  (local.set $idx (i32.load offset=24 (local.get $m)))
  (local.set $mask (i32.sub (i32.load offset=12 (local.get $idx)) (i32.const 1)))
  (local.set $h (i32.and (call $hash_str (local.get $k)) (local.get $mask)))
  (loop $next
    (local.set $e (i32.load offset=16 (i32.add (local.get $idx) (i32.shl (local.get $h) (i32.const 2)))))
    (if (i32.eqz (local.get $e)) (then (return (i32.const -1))))
    (if (call $str_eq (i32.load offset=16 (i32.add (i32.load offset=16 (local.get $m)) (i32.shl (i32.sub (local.get $e) (i32.const 1)) (i32.const 3)))) (local.get $k))
      (then (return (i32.sub (local.get $e) (i32.const 1)))))
    (local.set $h (i32.and (i32.add (local.get $h) (i32.const 1)) (local.get $mask)))
    (br $next))
  (i32.const -1))

(func $map_find_num (param $m i32) (param $k f64) (result i32)
  (local $idx i32) (local $mask i32) (local $h i32) (local $e i32)
  (if (i32.eqz (local.get $m)) (then (return (i32.const -1))))
  (local.set $idx (i32.load offset=24 (local.get $m)))
  (local.set $mask (i32.sub (i32.load offset=12 (local.get $idx)) (i32.const 1)))
  (local.set $h (i32.and (call $hash_num (local.get $k)) (local.get $mask)))
  (loop $next
    (local.set $e (i32.load offset=16 (i32.add (local.get $idx) (i32.shl (local.get $h) (i32.const 2)))))
    (if (i32.eqz (local.get $e)) (then (return (i32.const -1))))
    (if (f64.eq (f64.load offset=16 (i32.add (i32.load offset=16 (local.get $m)) (i32.shl (i32.sub (local.get $e) (i32.const 1)) (i32.const 3)))) (local.get $k))
      (then (return (i32.sub (local.get $e) (i32.const 1)))))
    (local.set $h (i32.and (i32.add (local.get $h) (i32.const 1)) (local.get $mask)))
    (br $next))
  (i32.const -1))

;; $map_append adds an entry for a key already written at the end of the keys store, and returns its index.
(func $map_append (param $m i32) (param $pos i32) (result i32)
  (local $n i32) (local $cap i32)
  (local.set $n (i32.load offset=12 (local.get $m)))
  (i32.store offset=12 (local.get $m) (i32.add (local.get $n) (i32.const 1)))
  (local.set $cap (i32.load offset=12 (i32.load offset=24 (local.get $m))))
  (if (i32.gt_u (i32.shl (i32.add (local.get $n) (i32.const 1)) (i32.const 1)) (local.get $cap))
    (then (call $map_reindex (local.get $m) (i32.shl (local.get $cap) (i32.const 1)) (local.get $pos)))
    (else (call $map_index (local.get $m) (local.get $n))))
  (local.get $n))

;; $map_room makes room for one more entry.
(func $map_room (param $m i32) (param $pos i32)
  (local $n i32)
  (local.set $n (i32.load offset=12 (local.get $m)))
  (if (i32.gt_u (i32.add (local.get $n) (i32.const 1)) (i32.const $MAX_LENGTH)) (then (call $fail_length (i32.const $S_map) (local.get $pos))))
  (i32.store offset=16 (local.get $m) (call $store_grow (i32.load offset=16 (local.get $m)) (local.get $n) (i32.add (local.get $n) (i32.const 1)) (local.get $pos)))
  (i32.store offset=20 (local.get $m) (call $store_grow (i32.load offset=20 (local.get $m)) (local.get $n) (i32.add (local.get $n) (i32.const 1)) (local.get $pos))))

;; $map_add_str and $map_add_num find the key's entry, adding one when there is none, and return where its value
;; goes.
(func $map_add_str (param $m i32) (param $k i32) (param $pos i32) (result i32)
  (local $i i32)
  (local.set $i (call $map_find_str (local.get $m) (local.get $k)))
  (if (i32.lt_s (local.get $i) (i32.const 0))
    (then
      (call $map_room (local.get $m) (local.get $pos))
      (i32.store offset=16 (i32.add (i32.load offset=16 (local.get $m)) (i32.shl (i32.load offset=12 (local.get $m)) (i32.const 3))) (local.get $k))
      (local.set $i (call $map_append (local.get $m) (local.get $pos)))))
  (i32.add (i32.add (i32.load offset=20 (local.get $m)) (i32.const 16)) (i32.shl (local.get $i) (i32.const 3))))

(func $map_add_num (param $m i32) (param $k f64) (param $pos i32) (result i32)
  (local $i i32)
  (local.set $i (call $map_find_num (local.get $m) (local.get $k)))
  (if (i32.lt_s (local.get $i) (i32.const 0))
    (then
      (call $map_room (local.get $m) (local.get $pos))
      (f64.store offset=16 (i32.add (i32.load offset=16 (local.get $m)) (i32.shl (i32.load offset=12 (local.get $m)) (i32.const 3))) (local.get $k))
      (local.set $i (call $map_append (local.get $m) (local.get $pos)))))
  (i32.add (i32.add (i32.load offset=20 (local.get $m)) (i32.const 16)) (i32.shl (local.get $i) (i32.const 3))))

;; $map_val is where entry i's value is.
(func $map_val (param $m i32) (param $i i32) (result i32)
  (i32.add (i32.add (i32.load offset=20 (local.get $m)) (i32.const 16)) (i32.shl (local.get $i) (i32.const 3))))

;; $map_remove takes entry i out, keeping the others in order.
(func $map_remove (param $m i32) (param $i i32) (param $pos i32)
  (local $n i32) (local $tail i32)
  (local.set $n (i32.sub (i32.load offset=12 (local.get $m)) (i32.const 1)))
  (local.set $tail (i32.shl (i32.sub (local.get $n) (local.get $i)) (i32.const 3)))
  (memory.copy
    (i32.add (i32.add (i32.load offset=16 (local.get $m)) (i32.const 16)) (i32.shl (local.get $i) (i32.const 3)))
    (i32.add (i32.add (i32.load offset=16 (local.get $m)) (i32.const 24)) (i32.shl (local.get $i) (i32.const 3)))
    (local.get $tail))
  (memory.copy
    (i32.add (i32.add (i32.load offset=20 (local.get $m)) (i32.const 16)) (i32.shl (local.get $i) (i32.const 3)))
    (i32.add (i32.add (i32.load offset=20 (local.get $m)) (i32.const 24)) (i32.shl (local.get $i) (i32.const 3)))
    (local.get $tail))
  (i32.store offset=12 (local.get $m) (local.get $n))
  (call $map_reindex (local.get $m) (i32.load offset=12 (i32.load offset=24 (local.get $m))) (local.get $pos)))

(func $map_del_str (param $m i32) (param $k i32) (param $pos i32) (result i32)
  (local $i i32)
  (call $cost (i32.load offset=12 (local.get $m)) (local.get $pos))
  (local.set $i (call $map_find_str (local.get $m) (local.get $k)))
  (if (i32.lt_s (local.get $i) (i32.const 0)) (then (return (i32.const 0))))
  (call $map_remove (local.get $m) (local.get $i) (local.get $pos))
  (i32.const 1))

(func $map_del_num (param $m i32) (param $k f64) (param $pos i32) (result i32)
  (local $i i32)
  (call $cost (i32.load offset=12 (local.get $m)) (local.get $pos))
  (local.set $i (call $map_find_num (local.get $m) (local.get $k)))
  (if (i32.lt_s (local.get $i) (i32.const 0)) (then (return (i32.const 0))))
  (call $map_remove (local.get $m) (local.get $i) (local.get $pos))
  (i32.const 1))

;; $map_list is keys() (vals 0) or values() (vals 1): a new array of tag.
(func $map_list (param $m i32) (param $vals i32) (param $tag i32) (param $pos i32) (result i32)
  (local $n i32) (local $out i32)
  (local.set $n (call $map_len (local.get $m)))
  (call $cost (local.get $n) (local.get $pos))
  (local.set $out (call $arr_new (local.get $tag) (local.get $n) (local.get $pos)))
  (if (local.get $n)
    (then (memory.copy
      (i32.add (i32.load offset=16 (local.get $out)) (i32.const 16))
      (i32.add (i32.load offset=16 (i32.add (local.get $m) (i32.shl (local.get $vals) (i32.const 2)))) (i32.const 16))
      (i32.shl (local.get $n) (i32.const 3)))))
  (local.get $out))

;; ---- handles: the objects the host holds, and native objects by the host's handle ----

(func $table_new (param $tag i32) (param $cap i32) (result i32)
  (local $t i32)
  (local.set $t (call $alloc (i32.add (i32.const 16) (i32.shl (local.get $cap) (i32.const 2))) (local.get $tag) (i32.const 0)))
  (i32.store offset=12 (local.get $t) (local.get $cap))
  (local.get $t))

;; $table_grow gives a table room for entry i, returning the table to use.
(func $table_grow (param $t i32) (param $i i32) (result i32)
  (local $cap i32) (local $nt i32)
  (local.set $cap (i32.load offset=12 (local.get $t)))
  (if (i32.lt_u (local.get $i) (local.get $cap)) (then (return (local.get $t))))
  (local.set $cap (i32.shl (local.get $cap) (i32.const 1)))
  (if (i32.le_u (local.get $cap) (local.get $i)) (then (local.set $cap (i32.add (local.get $i) (i32.const 1)))))
  (local.set $nt (call $table_new (i32.and (i32.load (local.get $t)) (i32.const 0x7fffffff)) (local.get $cap)))
  (memory.copy (i32.add (local.get $nt) (i32.const 16)) (i32.add (local.get $t) (i32.const 16)) (i32.shl (i32.load offset=12 (local.get $t)) (i32.const 2)))
  (local.get $nt))

(func $rt_init
  (global.set $handles (call $table_new (i32.const $T_HANDLES) (i32.const 16)))
  (global.set $natives (call $table_new (i32.const $T_NATIVES) (i32.const 16)))
  (global.set $hnext (i32.const 1)))

(func $__handle_new (param $p i32) (result i32)
  (local $h i32)
  (if (global.get $hfree)
    (then
      (local.set $h (global.get $hfree))
      (global.set $hfree (i32.shr_u (i32.load offset=16 (i32.add (global.get $handles) (i32.shl (local.get $h) (i32.const 2)))) (i32.const 1))))
    (else
      (local.set $h (global.get $hnext))
      (global.set $hnext (i32.add (local.get $h) (i32.const 1)))
      (global.set $handles (call $table_grow (global.get $handles) (local.get $h)))))
  (i32.store offset=16 (i32.add (global.get $handles) (i32.shl (local.get $h) (i32.const 2))) (local.get $p))
  (local.get $h))

(func $__handle_get (param $h i32) (result i32)
  (i32.load offset=16 (i32.add (global.get $handles) (i32.shl (local.get $h) (i32.const 2)))))

(func $__handle_drop (param $h i32)
  (i32.store offset=16 (i32.add (global.get $handles) (i32.shl (local.get $h) (i32.const 2)))
    (i32.or (i32.shl (global.get $hfree) (i32.const 1)) (i32.const 1)))
  (global.set $hfree (local.get $h)))

;; $__native_new makes the object of native class cls (its index) for the host's handle nh.
(func $__native_new (param $cls i32) (param $nh i32) (result i32)
  (local $p i32)
  (global.set $natives (call $table_grow (global.get $natives) (local.get $nh)))
  (local.set $p (call $alloc (i32.const 24) (i32.add (local.get $cls) (i32.const $T_CLASS)) (i32.const 0)))
  (i32.store offset=16 (local.get $p) (local.get $nh))
  (i32.store offset=16 (i32.add (global.get $natives) (i32.shl (local.get $nh) (i32.const 2))) (local.get $p))
  (local.get $p))

(func $__native_get (param $nh i32) (result i32)
  (i32.load offset=16 (i32.add (global.get $natives) (i32.shl (local.get $nh) (i32.const 2)))))

;; ---- what the host calls besides the script's own code ----

(func $__begin
  (global.set $steps (i32.const $MAX_STEPS))
  (global.set $depth (i32.const 0))
  (global.set $alloc (i32.const 0)))

;; $__end ends a call from the host: when the calls since the last collection allocated more than half the heap
;; budget, it collects and returns the bytes that are left; otherwise -1.
(func $__end (result i32)
  (global.set $since (i32.add (global.get $since) (global.get $alloc)))
  (global.set $alloc (i32.const 0))
  (if (i32.le_u (global.get $since) (i32.const $HALF_HEAP)) (then (return (i32.const -1))))
  (global.set $since (i32.const 0))
  (call $gc))

(func $__gc (result i32)
  (global.set $since (i32.const 0))
  (call $gc))

(func $__str_new (param $len i32) (result i32)
  (call $str_new (local.get $len) (global.get $npos)))

(func $__arr_new (param $tag i32) (param $n i32) (result i32)
  (call $arr_new (local.get $tag) (local.get $n) (global.get $npos)))

(func $__map_new (param $flags i32) (result i32)
  (call $map_new (local.get $flags) (global.get $npos)))

;; $__map_put sets the entry whose key is in scratch slot 0 to the value in slot 1.
(func $__map_put (param $m i32)
  (local $v i32)
  (if (i32.and (i32.load offset=28 (local.get $m)) (i32.const 1))
    (then (local.set $v (call $map_add_str (local.get $m) (i32.load offset=16 (i32.const 0)) (global.get $npos))))
    (else (local.set $v (call $map_add_num (local.get $m) (f64.load offset=16 (i32.const 0)) (global.get $npos)))))
  (i64.store (local.get $v) (i64.load offset=24 (i32.const 0))))

;; ---- the collector: sliding mark-compact, between calls only, so the roots are the handles and the module
;; constants ($gc_consts) and nothing on the stack ----

(func $gc_push (param $p i32)
  (local $t i32)
  (if (i32.lt_u (local.get $p) (global.get $heap)) (then (return)))
  (if (i32.and (local.get $p) (i32.const 1)) (then (return)))
  (local.set $t (i32.load (local.get $p)))
  (if (i32.and (local.get $t) (i32.const 0x80000000)) (then (return)))
  (i32.store (local.get $p) (i32.or (local.get $t) (i32.const 0x80000000)))
  (if (i32.eqz (call $grow_to (i32.add (global.get $msp) (i32.const 4)))) (then (call $fail_memory (i32.const 0))))
  (i32.store (global.get $msp) (local.get $p))
  (global.set $msp (i32.add (global.get $msp) (i32.const 4)))
  (if (i32.gt_u (global.get $msp) (global.get $msphi)) (then (global.set $msphi (global.get $msp)))))

;; $gc_slots pushes n 8-byte slots from p.
(func $gc_slots (param $p i32) (param $n i32)
  (local $i i32)
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (call $gc_push (i32.load (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3)))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next))))

;; $gc_scan pushes what the marked block p refers to.
(func $gc_scan (param $p i32)
  (local $t i32) (local $n i32) (local $i i32) (local $f i32) (local $info i32)
  (local.set $t (i32.and (i32.load (local.get $p)) (i32.const 0x7fffffff)))
  (if (i32.ge_u (local.get $t) (i32.const $T_CLASS))
    (then
      (local.set $info (i32.add (i32.const $CLASSES) (i32.shl (i32.sub (local.get $t) (i32.const $T_CLASS)) (i32.const 3))))
      (local.set $n (i32.load (local.get $info)))
      (local.set $f (i32.load offset=4 (local.get $info)))
      (block $done (loop $next
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (if (i32.load8_u (i32.add (local.get $f) (local.get $i)))
          (then (call $gc_push (i32.load offset=16 (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3)))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
      (return)))
  (if (i32.or (i32.eq (local.get $t) (i32.const $T_ARR_VALS)) (i32.eq (local.get $t) (i32.const $T_ARR_REFS)))
    (then
      (call $gc_push (i32.load offset=16 (local.get $p)))
      (if (i32.eq (local.get $t) (i32.const $T_ARR_REFS))
        (then (call $gc_slots (i32.add (i32.load offset=16 (local.get $p)) (i32.const 16)) (i32.load offset=12 (local.get $p)))))
      (return)))
  (if (i32.eq (local.get $t) (i32.const $T_MAP))
    (then
      (call $gc_push (i32.load offset=16 (local.get $p)))
      (call $gc_push (i32.load offset=20 (local.get $p)))
      (call $gc_push (i32.load offset=24 (local.get $p)))
      (local.set $f (i32.load offset=28 (local.get $p)))
      (if (i32.and (local.get $f) (i32.const 1))
        (then (call $gc_slots (i32.add (i32.load offset=16 (local.get $p)) (i32.const 16)) (i32.load offset=12 (local.get $p)))))
      (if (i32.and (local.get $f) (i32.const 2))
        (then (call $gc_slots (i32.add (i32.load offset=20 (local.get $p)) (i32.const 16)) (i32.load offset=12 (local.get $p)))))
      (return)))
  (if (i32.eq (local.get $t) (i32.const $T_HANDLES))
    (then
      (local.set $n (i32.load offset=12 (local.get $p)))
      (block $done (loop $next
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (call $gc_push (i32.load offset=16 (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 2)))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next))))))

(func $gc_fwd (param $p i32) (result i32)
  (if (result i32) (i32.or (i32.lt_u (local.get $p) (global.get $heap)) (i32.and (local.get $p) (i32.const 1)))
    (then (local.get $p))
    (else (i32.load offset=8 (local.get $p)))))

(func $gc_fix_slots (param $p i32) (param $n i32)
  (local $i i32) (local $a i32)
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
    (local.set $a (i32.add (local.get $p) (i32.shl (local.get $i) (i32.const 3))))
    (i32.store (local.get $a) (call $gc_fwd (i32.load (local.get $a))))
    (local.set $i (i32.add (local.get $i) (i32.const 1)))
    (br $next))))

;; $gc_fix points the marked block p's references where their blocks are going; dead native objects are dropped
;; from the natives table and their handles given back to the host.
(func $gc_fix (param $p i32)
  (local $t i32) (local $n i32) (local $i i32) (local $f i32) (local $info i32) (local $a i32) (local $e i32)
  (local.set $t (i32.and (i32.load (local.get $p)) (i32.const 0x7fffffff)))
  (if (i32.ge_u (local.get $t) (i32.const $T_CLASS))
    (then
      (local.set $info (i32.add (i32.const $CLASSES) (i32.shl (i32.sub (local.get $t) (i32.const $T_CLASS)) (i32.const 3))))
      (local.set $n (i32.load (local.get $info)))
      (local.set $f (i32.load offset=4 (local.get $info)))
      (block $done (loop $next
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (if (i32.load8_u (i32.add (local.get $f) (local.get $i)))
          (then
            (local.set $a (i32.add (i32.add (local.get $p) (i32.const 16)) (i32.shl (local.get $i) (i32.const 3))))
            (i32.store (local.get $a) (call $gc_fwd (i32.load (local.get $a))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
      (return)))
  (if (i32.or (i32.eq (local.get $t) (i32.const $T_ARR_VALS)) (i32.eq (local.get $t) (i32.const $T_ARR_REFS)))
    (then
      (if (i32.eq (local.get $t) (i32.const $T_ARR_REFS))
        (then (call $gc_fix_slots (i32.add (i32.load offset=16 (local.get $p)) (i32.const 16)) (i32.load offset=12 (local.get $p)))))
      (i32.store offset=16 (local.get $p) (call $gc_fwd (i32.load offset=16 (local.get $p))))
      (return)))
  (if (i32.eq (local.get $t) (i32.const $T_MAP))
    (then
      (local.set $f (i32.load offset=28 (local.get $p)))
      (if (i32.and (local.get $f) (i32.const 1))
        (then (call $gc_fix_slots (i32.add (i32.load offset=16 (local.get $p)) (i32.const 16)) (i32.load offset=12 (local.get $p)))))
      (if (i32.and (local.get $f) (i32.const 2))
        (then (call $gc_fix_slots (i32.add (i32.load offset=20 (local.get $p)) (i32.const 16)) (i32.load offset=12 (local.get $p)))))
      (i32.store offset=16 (local.get $p) (call $gc_fwd (i32.load offset=16 (local.get $p))))
      (i32.store offset=20 (local.get $p) (call $gc_fwd (i32.load offset=20 (local.get $p))))
      (i32.store offset=24 (local.get $p) (call $gc_fwd (i32.load offset=24 (local.get $p))))
      (return)))
  (if (i32.eq (local.get $t) (i32.const $T_HANDLES))
    (then
      (local.set $n (i32.load offset=12 (local.get $p)))
      (block $done (loop $next
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $a (i32.add (i32.add (local.get $p) (i32.const 16)) (i32.shl (local.get $i) (i32.const 2))))
        (i32.store (local.get $a) (call $gc_fwd (i32.load (local.get $a))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
      (return)))
  (if (i32.eq (local.get $t) (i32.const $T_NATIVES))
    (then
      (local.set $n (i32.load offset=12 (local.get $p)))
      (block $done (loop $next
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $a (i32.add (i32.add (local.get $p) (i32.const 16)) (i32.shl (local.get $i) (i32.const 2))))
        (local.set $e (i32.load (local.get $a)))
        (if (local.get $e)
          (then
            (if (i32.and (i32.load (local.get $e)) (i32.const 0x80000000))
              (then (i32.store (local.get $a) (call $gc_fwd (local.get $e))))
              (else
                (i32.store (local.get $a) (i32.const 0))
                (call $imp_release (local.get $i))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next))))))

;; $gc collects: it marks what the roots reach, works out where each live block goes, points every reference
;; there, slides the blocks down, and clears what is free. It returns the live heap's size.
(func $gc (result i32)
  (local $scan i32) (local $free i32) (local $t i32) (local $size i32) (local $base i32) (local $dest i32)
  (local.set $base (global.get $hp))
  (global.set $msp (local.get $base))
  (global.set $msphi (local.get $base))
  (call $gc_push (global.get $handles))
  (call $gc_push (global.get $natives))
  (call $gc_consts (i32.const 0))
  (block $marked (loop $more
    (br_if $marked (i32.le_u (global.get $msp) (local.get $base)))
    (global.set $msp (i32.sub (global.get $msp) (i32.const 4)))
    (call $gc_scan (i32.load (global.get $msp)))
    (br $more)))
  ;; where each live block goes
  (local.set $scan (global.get $heap))
  (local.set $free (global.get $heap))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $scan) (global.get $hp)))
    (local.set $size (i32.load offset=4 (local.get $scan)))
    (if (i32.and (i32.load (local.get $scan)) (i32.const 0x80000000))
      (then
        (i32.store offset=8 (local.get $scan) (local.get $free))
        (local.set $free (i32.add (local.get $free) (local.get $size)))))
    (local.set $scan (i32.add (local.get $scan) (local.get $size)))
    (br $next)))
  ;; references
  (call $gc_consts (i32.const 1))
  (local.set $scan (global.get $heap))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $scan) (global.get $hp)))
    (if (i32.and (i32.load (local.get $scan)) (i32.const 0x80000000)) (then (call $gc_fix (local.get $scan))))
    (local.set $scan (i32.add (local.get $scan) (i32.load offset=4 (local.get $scan))))
    (br $next)))
  (global.set $handles (call $gc_fwd (global.get $handles)))
  (global.set $natives (call $gc_fwd (global.get $natives)))
  ;; slide
  (local.set $scan (global.get $heap))
  (block $done (loop $next
    (br_if $done (i32.ge_u (local.get $scan) (global.get $hp)))
    (local.set $t (i32.load (local.get $scan)))
    (local.set $size (i32.load offset=4 (local.get $scan)))
    (if (i32.and (local.get $t) (i32.const 0x80000000))
      (then
        (local.set $dest (i32.load offset=8 (local.get $scan)))
        (i32.store (local.get $scan) (i32.and (local.get $t) (i32.const 0x7fffffff)))
        (if (i32.ne (local.get $dest) (local.get $scan))
          (then (memory.copy (local.get $dest) (local.get $scan) (local.get $size))))))
    (local.set $scan (i32.add (local.get $scan) (local.get $size)))
    (br $next)))
  (memory.fill (local.get $free) (i32.const 0) (i32.sub (global.get $msphi) (local.get $free)))
  (global.set $hp (local.get $free))
  (i32.sub (local.get $free) (global.get $heap)))
