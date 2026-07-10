package main

// context.go — the context-window gauge's arithmetic: the model→window table
// and the clamp that turns a raw token count (parsed off the session JSONL in
// tail.go) into the 0–100 percentage the phone draws as a bar.
//
// THE ONE THING TO UPDATE PER MODEL is contextWindows below: when a model ships
// with a context window other than the conservative default, add its id here.
// Everything absent falls to defaultContextWindow — deliberately the SMALL 200k
// window, so an unrecognized model OVER-reports pressure rather than
// under-reporting by assuming a huge window it may not actually have.

// contextWindows maps a Claude Code model id (message.model in the session
// JSONL, e.g. "claude-opus-4-8") to its context-window size in tokens. Model
// ids are plain, undated lowercase strings in practice (verified against live
// sessions), so an exact-match table is enough. Add one row per model whose
// window differs from the default; anything absent uses defaultContextWindow.
var contextWindows = map[string]int{
	"claude-opus-4-8": 1_000_000,
	// Found live 2026-07-10: Vint ran at a true 40% (397k of Fable's 1M) while
	// the gauge pinned at 100% — Fable was absent here, so his 397k was divided
	// by the conservative 200k default (198% → clamp). The table was born inside
	// an Opus session (#23, July 8) and nobody told it about the brain swap.
	"claude-fable-5": 1_000_000,
}

// defaultContextWindow is the window assumed for any model absent from the
// table (including "" and synthetic placeholders). 200k is the conservative
// floor — the smallest common Claude window — so an unknown model can never
// *under*-report pressure by pretending its window is larger than it is.
const defaultContextWindow = 200_000

// contextWindowFor returns the context-window size (tokens) for a model id,
// falling back to the conservative defaultContextWindow for anything not in the
// table.
func contextWindowFor(model string) int {
	if w, ok := contextWindows[model]; ok {
		return w
	}
	return defaultContextWindow
}

// contextPct turns a raw current-context token count and its model into a
// clamped 0–100 percentage of the model's window. It returns 0 — which
// /api/status hides via omitempty — when there is nothing meaningful to show
// (no tokens parsed yet, or an impossible window), so "unknown" reads as absent
// rather than as a present-but-empty 0% bar.
func contextPct(tokens int, model string) int {
	window := contextWindowFor(model)
	if tokens <= 0 || window <= 0 {
		return 0
	}
	pct := tokens * 100 / window
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}
