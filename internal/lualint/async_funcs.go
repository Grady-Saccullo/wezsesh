package lualint

// asyncFuncs is the set of wezterm Lua APIs that are forbidden inside
// the synchronous portion of `user-var-changed` (steps a–h, §14.3) — a
// call to any of these between markers `(a)` and `(h)` of `ipc.lua`
// fails the §17.4 "Lua handler `.await`-free" lint.
//
// Names are matched against the token stream as fully-qualified dotted
// paths (e.g. "wezterm.run_child_process") so a local rebinding like
// `local rcp = wezterm.run_child_process` will need its own data-flow
// analysis in T-600 — out of scope for this skeleton, which only
// provides the data.
//
// `wezterm.background_child_process` is intentionally NOT in this set:
// per §14.3 it is fire-and-forget and permitted in step (i) only. The
// pcall-wrap requirement on it is a separate rule (§16.5) that lives in
// T-005.
//
// The set seeded here is exactly §14.3's explicit enumeration. §14.3
// also says "any add_async_function-exposed API enumerated in
// internal/lualint/async_funcs.go" — additions to that broader surface
// are deferred to T-600, which audits the upstream wezterm release
// pinned by §16.4 and adds entries with citations. Front-running that
// audit risks false-positive build failures.
var asyncFuncs = map[string]bool{
	"wezterm.run_child_process": true,
	"wezterm.sleep_ms":          true,
}

// IsAsync reports whether name is a known-async wezterm API. name is
// the fully-qualified dotted path the call appears under in source
// (e.g. "wezterm.run_child_process"). Unknown names return false; the
// list intentionally errs on the side of under-flagging — false
// positives in this lint block legitimate plugin code.
func IsAsync(name string) bool {
	return asyncFuncs[name]
}

// AsyncFuncs returns a copy of the registered async-function set as a
// slice. Order is unspecified. Used by tests and by tooling that wants
// to print the current registry; callers MUST NOT rely on ordering or
// on completeness across releases.
func AsyncFuncs() []string {
	out := make([]string, 0, len(asyncFuncs))
	for k := range asyncFuncs {
		out = append(out, k)
	}
	return out
}
