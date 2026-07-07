package main

// paper.go — The Bridge Herald: the daemon's own morning newspaper.
//
// Once a day (config paper_hour, default 7, local time; -1 disables), the
// daemon composes an edition from the event history since the last one and
// posts it to #crew as a single "paper" event, with one push. It is written
// for a reader who was asleep: what the crew did, what needs their eyes,
// what the house noticed overnight. Composed DETERMINISTICALLY from recorded
// facts — the daemon reports, it never invents. The paper renders only on
// the phone (no pane fan-out): agents are not woken at dawn for a newspaper,
// and the one place it must land softly is a new parent's lock screen.
//
// `bridge paper` (POST /local/paper) prints an edition on demand; a manual
// edition counts as today's, so the scheduled one stands down until tomorrow.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const paperDefaultHour = 7

// paperState is the persisted memory of the press: when the last edition ran
// and its number. Loaded at startup, rewritten after every edition.
type paperState struct {
	LastUnix int64 `json:"last_unix"`
	Edition  int   `json:"edition"`
}

var paper paperState

func paperPath() string { return bridgePath("paper.json") }

// loadPaperState restores the press's memory. Called once from runServe.
func loadPaperState() {
	if data, err := os.ReadFile(paperPath()); err == nil {
		_ = json.Unmarshal(data, &paper)
	}
}

func savePaperState() {
	data, _ := json.Marshal(paper)
	_ = writeFilePrivate(paperPath(), data)
}

// paperHour resolves the configured publication hour: config paper_hour,
// default 7, negative disables the scheduled edition entirely (manual
// `bridge paper` still works).
func paperHour() int {
	if authConfig.PaperHour != nil {
		return *authConfig.PaperHour
	}
	return paperDefaultHour
}

// paperDue reports whether today's edition hasn't run yet and its hour has
// passed. Called every reconcile tick; cheap by construction.
//
// A press with no memory (fresh install, LastUnix zero) does NOT fire at
// boot: the first check stamps the install moment and the schedule starts
// with tomorrow's hour — nobody wants a newspaper mid-handshake. `bridge
// paper` prints one on demand any time.
func paperDue(now time.Time) bool {
	if paper.LastUnix == 0 {
		paper.LastUnix = now.Unix()
		savePaperState()
		return false
	}
	h := paperHour()
	if h < 0 {
		return false
	}
	todays := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
	return now.After(todays) && paper.LastUnix < todays.Unix()
}

// publishPaper composes and files an edition covering everything since the
// previous one (first edition: the last 24h), then remembers it ran. The
// paper is keyed to #crew — the crew's shared thread is where the crew's
// newspaper belongs — and pushed once, tagged "paper" so a slow morning
// double-tap replaces rather than stacks.
func publishPaper(now time.Time) {
	since := paper.LastUnix
	if since == 0 {
		since = now.Add(-24 * time.Hour).Unix()
	}
	paper.Edition++
	paper.LastUnix = now.Unix()
	savePaperState()

	body := composePaper(since, now)
	name := fmt.Sprintf("The Bridge Herald · Edition %d · %s", paper.Edition, now.Format("Mon Jan 2"))
	emitEvent(Event{Type: "paper", Agent: roomCrewID, Name: name, Text: body})
	notifyPush("☕ The Bridge Herald", paperHeadline(body), "paper", roomCrewID)
	audit("paper", fmt.Sprintf("edition %d (%d bytes)", paper.Edition, len(body)), "daemon")
}

// paperHeadline is the push body: the first bullet of the wire, or a quiet
// default for a night when nothing happened (the best kind, some weeks).
func paperHeadline(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "• ") {
			return truncateRunes(strings.TrimPrefix(ln, "• "), 140)
		}
	}
	return "a quiet night on the bridge"
}

// composePaper writes the edition from recorded facts in [since, now):
// the overnight wire (what each agent said, how much), what needs the
// reader's eyes right now, the status board, and the house notes. Sections
// with nothing to say are omitted — a newspaper, not a form.
func composePaper(since int64, now time.Time) string {
	events := eventsSince(0)
	type tally struct {
		replies, heard int    // agent words out, user words in
		lastLine       string // last printable non-thinking line
	}
	wire := map[string]*tally{}
	roomPosts := 0
	for _, ev := range events {
		// Window (since, now]: edition N covered up TO its own stamp, so this
		// one starts strictly after it — and includes events sharing the
		// publish second (an exclusive upper bound once hid a whole burst
		// that landed in the same second as the press run; caught in smoke).
		ts, err := time.Parse(time.RFC3339, ev.TS)
		if err != nil || ts.Unix() <= since || ts.Unix() > now.Unix() {
			continue
		}
		switch ev.Type {
		case "reply":
			t := wire[ev.Name]
			if t == nil {
				t = &tally{}
				wire[ev.Name] = t
			}
			t.replies++
			if line := printableLine(ev.Text); line != "" {
				t.lastLine = line
			}
		case "sent":
			if ev.Agent == roomCrewID {
				roomPosts++
			} else if c := registry.Resolve(ev.Agent); c != nil {
				t := wire[c.Name]
				if t == nil {
					t = &tally{}
					wire[c.Name] = t
				}
				t.heard++
			}
		case "peer":
			if ev.Agent == roomCrewID {
				roomPosts++
			}
		}
	}

	var b strings.Builder

	// THE OVERNIGHT WIRE — one bullet per agent that spoke, busiest first.
	names := make([]string, 0, len(wire))
	for n := range wire {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return wire[names[i]].replies > wire[names[j]].replies })
	// A night of only party-line chatter is still news: the wire prints when
	// ANY words moved, 1:1 or room.
	if len(names) > 0 || roomPosts > 0 {
		b.WriteString("THE OVERNIGHT WIRE\n")
		for _, n := range names {
			t := wire[n]
			line := fmt.Sprintf("• %s — %d %s", n, t.replies, plural(t.replies, "message"))
			if t.heard > 0 {
				line += fmt.Sprintf(", %d from you", t.heard)
			}
			if t.lastLine != "" {
				line += fmt.Sprintf(". Last word: “%s”", truncateRunes(t.lastLine, 90))
			}
			b.WriteString(line + "\n")
		}
		if roomPosts > 0 {
			b.WriteString(fmt.Sprintf("• #crew — %d %s on the party line\n", roomPosts, plural(roomPosts, "post")))
		}
		b.WriteString("\n")
	}

	// NEEDS YOUR EYES — open prompts right now, oldest first. The one section
	// that must never be omitted when non-empty.
	type waiting struct {
		name string
		age  time.Duration
	}
	var waits []waiting
	for _, c := range registry.Roster() {
		if c.PromptOpen && c.PromptSince > 0 {
			waits = append(waits, waiting{c.Name, time.Duration(now.Unix()-c.PromptSince) * time.Second})
		}
	}
	sort.Slice(waits, func(i, j int) bool { return waits[i].age > waits[j].age })
	if len(waits) > 0 {
		b.WriteString("NEEDS YOUR EYES\n")
		for _, w := range waits {
			b.WriteString(fmt.Sprintf("• %s has a permission prompt open (%s)\n", w.name, humanDur(w.age)))
		}
		b.WriteString("\n")
	}

	// STATUS BOARD — away lines as the crew set them.
	var stats []string
	for _, c := range registry.Roster() {
		if c.Away != "" {
			stats = append(stats, fmt.Sprintf("• %s: %s", c.Name, c.Away))
		}
	}
	if len(stats) > 0 {
		sort.Strings(stats)
		b.WriteString("STATUS BOARD\n" + strings.Join(stats, "\n") + "\n\n")
	}

	// HOUSE NOTES — what the building itself noticed.
	var notes []string
	if from, to := wakeGap(); to > since && from > 0 {
		f, t := time.Unix(from, 0), time.Unix(to, 0)
		notes = append(notes, fmt.Sprintf("• the Mac slept %s–%s (%s)", f.Format("15:04"), t.Format("15:04"), humanDur(t.Sub(f))))
	}
	if daemonStartUnix > since {
		notes = append(notes, fmt.Sprintf("• the daemon came up at %s (deploy or restart)", time.Unix(daemonStartUnix, 0).Format("15:04")))
	}
	live := 0
	for _, c := range registry.Roster() {
		if c.Status == "live" {
			live++
		}
	}
	notes = append(notes, fmt.Sprintf("• %d %s on the roster, all wires quiet and held", live, plural(live, "agent")))
	b.WriteString("HOUSE NOTES\n" + strings.Join(notes, "\n") + "\n")

	if len(names) == 0 && roomPosts == 0 && len(waits) == 0 {
		return "A quiet night — nothing happened, and everything held. The best kind of edition.\n\n" + b.String()
	}
	return b.String()
}

// printableLine returns the first line of agent text fit for print: thinking
// spans removed (an agent's visible reasoning is house culture, not front-page
// copy), markers dropped, whitespace collapsed. Empty when nothing survives.
func printableLine(text string) string {
	for {
		start := strings.Index(text, "[thinking]")
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], "[end-thinking]")
		if end < 0 {
			text = text[:start]
			break
		}
		text = text[:start] + text[start+end+len("[end-thinking]"):]
	}
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(strings.ReplaceAll(ln, "**", ""))
		if ln != "" && !strings.HasPrefix(ln, "[") {
			return ln
		}
	}
	return ""
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
