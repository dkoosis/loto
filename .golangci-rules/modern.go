// modern.go — the modern-Go idiom half of the lintbrush ruleguard pack
// (ccp-p016). Pattern rules for the Go 1.21–1.26 idioms the use-modern-go
// skill lists but golangci's modernize / intrange / usetesting / errorlint /
// usestdlibvars suite does not enforce, so every repo running the pack gets
// them at the gate instead of at write time.
//
// Scope is deliberately mechanical: every rule here is a pure AST shape with
// one stdlib replacement. Idioms that need intent (cmp.Or over a chain of
// zero-checks, errors.Join, context causes, ServeMux method patterns, the
// time.Tick GC change) stay with the skill — they are not pattern-shaped.
// The maps.Copy loop is absent on purpose: golangci-lint v2.12.2's bundled
// modernize (mapsloop) already reports it, and every consumer runs modernize.
//
// Like rules.go this file is DSL, not a program: ruleguard parses it, `go
// build` never compiles it. A consuming repo copies it verbatim to
// .golangci-rules/modern.go and widens its ruleguard `rules` glob to include
// it. Each rule carries a Report naming the replacement and, where the
// rewrite is a pure substitution, a Suggest golangci-lint --fix can apply.
// README.md carries the per-rule table with the use-modern-go guideline id.
//
//pack:version v1
package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// --- time.Until / time.Since -----------------------------------------------

//doc:summary $x.Sub(time.Now()) spelled out instead of time.Until
//doc:before  deadline.Sub(time.Now())
//doc:after   time.Until(deadline)
//doc:tags    modern
func timeUntil(m dsl.Matcher) {
	m.Match(`$x.Sub(time.Now())`).
		Where(m["x"].Type.Is(`time.Time`)).
		Suggest(`time.Until($x)`).
		Report(`timeUntil: $x.Sub(time.Now()) is time.Until($x)`)
}

//doc:summary time.Now().Sub($x) spelled out instead of time.Since
//doc:before  time.Now().Sub(start)
//doc:after   time.Since(start)
//doc:tags    modern
func timeSince(m dsl.Matcher) {
	m.Match(`time.Now().Sub($x)`).
		Suggest(`time.Since($x)`).
		Report(`timeSince: time.Now().Sub($x) is time.Since($x)`)
}

// --- slices.Clone -----------------------------------------------------------

//doc:summary hand-rolled slice copy via append onto an empty slice
//doc:before  append([]T(nil), s...)
//doc:after   slices.Clone(s)
//doc:tags    modern
func slicesClone(m dsl.Matcher) {
	// Three spellings of the pre-1.21 idiom: append onto a nil conversion,
	// onto an empty literal, or onto the zero-cap reslice of the source.
	// $s must itself be a slice: append([]byte(nil), str...) copies a string
	// and has no slices.Clone form.
	m.Match(
		`append([]$_(nil), $s...)`,
		`append([]$_{}, $s...)`,
		`append($s[:0:0], $s...)`,
	).
		Where(m["s"].Type.Is(`[]$_`)).
		Suggest(`slices.Clone($s)`).
		Report(`slicesClone: append onto an empty slice is slices.Clone($s)`)
}

// --- reflect.TypeFor --------------------------------------------------------

//doc:summary reflect.TypeOf((*T)(nil)).Elem() instead of reflect.TypeFor[T]()
//doc:before  reflect.TypeOf((*io.Reader)(nil)).Elem()
//doc:after   reflect.TypeFor[io.Reader]()
//doc:tags    modern
func reflectTypeFor(m dsl.Matcher) {
	m.Match(`reflect.TypeOf((*$t)(nil)).Elem()`).
		Suggest(`reflect.TypeFor[$t]()`).
		Report(`reflectTypeFor: reflect.TypeOf((*$t)(nil)).Elem() is reflect.TypeFor[$t]()`)
}

// --- strings.Cut / bytes.Cut ------------------------------------------------

//doc:summary Index + slice on both sides of the separator instead of Cut
//doc:before  i := strings.Index(s, sep); before, after := s[:i], s[i+len(sep):]
//doc:after   before, after, found := strings.Cut(s, sep)
//doc:tags    modern
func stringsCut(m dsl.Matcher) {
	// The shape: an Index result used to slice the SAME string past the
	// separator. Slicing past the separator (i+len(sep), or i+1 for a
	// one-byte separator) is the tell — that is Cut's `after`. A bare s[:i]
	// or s[i:] alone is not flagged: those keep the separator and have no
	// direct Cut spelling. Statement-list patterns, so the Index call and
	// the slice must sit in the same block; an intervening guard on $i
	// (if $i < 0 { return }) is absorbed by $*_.
	m.Match(
		`$i := strings.Index($s, $sep); $*_; $_ = $s[$i+len($sep):]`,
		`$i := strings.Index($s, $sep); $*_; $_ := $s[$i+len($sep):]`,
		`$i := strings.Index($s, $sep); $*_; $_, $_ = $s[:$i], $s[$i+len($sep):]`,
		`$i := strings.Index($s, $sep); $*_; $_, $_ := $s[:$i], $s[$i+len($sep):]`,
		`$i := strings.Index($s, $sep); $*_; $_, $_ = $s[:$i], $s[$i+1:]`,
		`$i := strings.Index($s, $sep); $*_; $_, $_ := $s[:$i], $s[$i+1:]`,
		`$i := strings.IndexByte($s, $sep); $*_; $_, $_ = $s[:$i], $s[$i+1:]`,
		`$i := strings.IndexByte($s, $sep); $*_; $_, $_ := $s[:$i], $s[$i+1:]`,
	).
		Report(`stringsCut: Index then slice around the separator is strings.Cut($s, $sep)`)

	m.Match(
		`$i := bytes.Index($s, $sep); $*_; $_ = $s[$i+len($sep):]`,
		`$i := bytes.Index($s, $sep); $*_; $_ := $s[$i+len($sep):]`,
		`$i := bytes.Index($s, $sep); $*_; $_, $_ = $s[:$i], $s[$i+len($sep):]`,
		`$i := bytes.Index($s, $sep); $*_; $_, $_ := $s[:$i], $s[$i+len($sep):]`,
		`$i := bytes.IndexByte($s, $sep); $*_; $_, $_ = $s[:$i], $s[$i+1:]`,
		`$i := bytes.IndexByte($s, $sep); $*_; $_, $_ := $s[:$i], $s[$i+1:]`,
	).
		Report(`stringsCut: Index then slice around the separator is bytes.Cut($s, $sep)`)
}

// --- typed atomics ----------------------------------------------------------

//doc:summary sync/atomic function on a plain integer/pointer instead of a typed atomic
//doc:before  atomic.AddInt64(&c.n, 1)
//doc:after   c.n.Add(1)   // n atomic.Int64
//doc:tags    modern
func typedAtomic(m dsl.Matcher) {
	// The address-of argument is the tell: a typed atomic (atomic.Int64,
	// atomic.Pointer[T], …) is called as a method and never passed by &.
	// No Suggest — the fix is a field type change, not a call rewrite.
	m.Match(
		`atomic.LoadInt32(&$x)`, `atomic.LoadInt64(&$x)`,
		`atomic.LoadUint32(&$x)`, `atomic.LoadUint64(&$x)`,
		`atomic.LoadUintptr(&$x)`, `atomic.LoadPointer(&$x)`,
		`atomic.StoreInt32(&$x, $_)`, `atomic.StoreInt64(&$x, $_)`,
		`atomic.StoreUint32(&$x, $_)`, `atomic.StoreUint64(&$x, $_)`,
		`atomic.StoreUintptr(&$x, $_)`, `atomic.StorePointer(&$x, $_)`,
		`atomic.AddInt32(&$x, $_)`, `atomic.AddInt64(&$x, $_)`,
		`atomic.AddUint32(&$x, $_)`, `atomic.AddUint64(&$x, $_)`,
		`atomic.AddUintptr(&$x, $_)`,
		`atomic.SwapInt32(&$x, $_)`, `atomic.SwapInt64(&$x, $_)`,
		`atomic.SwapUint32(&$x, $_)`, `atomic.SwapUint64(&$x, $_)`,
		`atomic.SwapUintptr(&$x, $_)`, `atomic.SwapPointer(&$x, $_)`,
		`atomic.CompareAndSwapInt32(&$x, $_, $_)`, `atomic.CompareAndSwapInt64(&$x, $_, $_)`,
		`atomic.CompareAndSwapUint32(&$x, $_, $_)`, `atomic.CompareAndSwapUint64(&$x, $_, $_)`,
		`atomic.CompareAndSwapUintptr(&$x, $_, $_)`, `atomic.CompareAndSwapPointer(&$x, $_, $_)`,
	).
		Report(`typedAtomic: atomic function on &$x; declare $x as a typed atomic (atomic.Int64, atomic.Pointer[T], …) and call its method`)
}

// --- sync.OnceFunc / sync.OnceValue -----------------------------------------

//doc:summary method whose whole body is once.Do(closure) instead of sync.OnceFunc/OnceValue
//doc:before  func (c *C) init() { c.once.Do(func() { ... }) }
//doc:after   init := sync.OnceFunc(func() { ... })
//doc:tags    modern
func syncOnceFunc(m dsl.Matcher) {
	// A method (or function) whose entire body is a single Do call with a
	// closure is the pre-1.21 spelling of sync.OnceFunc; the same with a
	// trailing `return field` is sync.OnceValue. The sync.Once field plus
	// the guard method collapse into one func value. A Do call that sits
	// among other statements is left alone — that is a real use of Once.
	m.Match(
		`func ($_ $_) $_() { $o.Do(func() { $*_ }) }`,
		`func $_() { $o.Do(func() { $*_ }) }`,
	).
		Where(m["o"].Type.Is(`sync.Once`) || m["o"].Type.Is(`*sync.Once`)).
		Report(`syncOnceFunc: a method that only calls $o.Do(closure) is a sync.OnceFunc value`)

	m.Match(
		`func ($_ $_) $_() $_ { $o.Do(func() { $*_ }); return $_ }`,
		`func $_() $_ { $o.Do(func() { $*_ }); return $_ }`,
	).
		Where(m["o"].Type.Is(`sync.Once`) || m["o"].Type.Is(`*sync.Once`)).
		Report(`syncOnceFunc: a method that only calls $o.Do(closure) then returns is a sync.OnceValue value`)
}
