package tooltopology

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// ServeDoorMap returns the block a harness receives at session start. It never
// fails and never blocks on anything but one bounded file read: a missing,
// corrupt or stale snapshot becomes a short visible notice, because a silent
// gap would read as "no doors exist" and a stale table would assert statuses
// nobody re-checked.
func ServeDoorMap(home string, now time.Time, ttl time.Duration) string {
	if ttl <= 0 {
		ttl = DefaultDoorMapTTL
	}
	m, err := LoadDoorMap(home)
	if errors.Is(err, os.ErrNotExist) {
		return fitBlock("## Door map: none produced yet. Observe the machine once with `" + servedRefreshHint + " --doctor-json <door-report>`; until then rediscover doors by hand.")
	}
	if err != nil {
		return fitBlock("## Door map: snapshot unreadable (" + cleanField(err.Error(), 90) + "). Rebuild: `" + servedRefreshHint + "`.")
	}
	age, basis, ok := observedAge(m, now)
	if !ok {
		return fitBlock("## Door map: snapshot has no usable timestamp; treat as unverified. Rebuild: `" + servedRefreshHint + "`.")
	}
	if age > ttl {
		return fitBlock(fmt.Sprintf("## Door map STALE: %s old (door TTL %s). Statuses withheld because they may have flipped since (logins expire, devices unplug). Refresh: `%s --doctor-json <door-report>`; last full snapshot: `compound doors show`.",
			humanAge(age), humanAge(ttl), servedRefreshHint))
	}
	return RenderDoorMap(m, now, basis)
}

// observedAge is how old the DOOR statuses are, falling back to the
// snapshot time when the doors section carries none.
func observedAge(m DoorMap, now time.Time) (time.Duration, time.Time, bool) {
	for _, ts := range []string{m.DoorsObservedAt, m.GeneratedAt} {
		if ts == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			age := now.Sub(t)
			if age < 0 {
				age = 0
			}
			return age, t, true
		}
	}
	return 0, time.Time{}, false
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Section byte budgets. They stay under MaxServedBytes; RenderDoorMap hands the
// rest to doors and models, and the final fitBlock cap is the backstop that
// makes the limit unconditional. Harness versions are not served: they are
// for `compound doors show`, not for acting on the machine.
const (
	budgetHeader  = 110
	budgetDoors   = 1900
	budgetMCP     = 290
	budgetModels  = 150
	budgetChanged = 110
	// maxRecipeBytes caps one PASS line when the recipes do not all fit.
	maxRecipeBytes = 150
)

// RenderDoorMap renders the served block for a FRESH snapshot within MaxServedBytes.
// Pass one uses the base section budgets; whatever the cap has left over is then
// handed to the sections that lose the most information to truncation (door
// recipes and the launcher model list). If the enlarged render does not fit,
// the base render is served.
func RenderDoorMap(m DoorMap, now, observed time.Time) string {
	out := renderDoorMapWith(m, now, observed, 0, 0)
	slack := MaxServedBytes - len(out) - 8
	if slack <= 0 {
		return fitBlock(out)
	}
	if bigger := renderDoorMapWith(m, now, observed, slack, 0); len(bigger) <= MaxServedBytes {
		out = bigger
	} else {
		slack = 0
	}
	// A non-PASS line is cut at badLineBytes first. Bytes the cap still leaves
	// unused go to the cut ones, so a door's cause is not cut while room remains.
	// A line that becomes whole frees the rest of its share, so the share is
	// handed out again until no cut line can grow. A cut that lands after a
	// space can shift a line by a byte or two, so an overshoot halves the step.
	extra := 0
	for round := 0; round < 12; round++ {
		cut := cutNonPassDoors(m.Doors, badLineBytes+extra)
		if cut == 0 {
			break
		}
		step := (MaxServedBytes - len(out)) / cut
		for step > 0 {
			if grown := renderDoorMapWith(m, now, observed, slack, extra+step); len(grown) <= MaxServedBytes {
				out, extra = grown, extra+step
				break
			}
			step /= 2
		}
		if step == 0 {
			break
		}
	}
	return fitBlock(out)
}

func renderDoorMapWith(m DoorMap, now, observed time.Time, slack, badExtra int) string {
	var lines []string
	lines = append(lines, truncBytes(fmt.Sprintf("## Door map (cached, not live; doors checked %s ago at %s). Full: compound doors show",
		humanAge(now.Sub(observed)), observed.UTC().Format("2006-01-02T15:04Z")), budgetHeader))
	lines = append(lines, renderDoorLines(m.Doors, budgetDoors+slack/2, badExtra)...)
	if l := renderMCPLine(m.MCP, budgetMCP); l != "" {
		lines = append(lines, l)
	}
	if l := renderModelLine(m.Models, budgetModels+slack/2); l != "" {
		lines = append(lines, l)
	}
	if l := renderChangedLine(m.RecentChanges, now, budgetChanged); l != "" {
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// fitBlock is the unconditional cap.
func fitBlock(s string) string {
	if len(s) <= MaxServedBytes {
		return s
	}
	return truncBytes(s, MaxServedBytes)
}

// badLineBytes caps a non-PASS line in the base render.
const badLineBytes = 96

func nonPassLine(d Door) string {
	return fmt.Sprintf("%s %s: %s", strings.ToUpper(d.Status), d.Name, firstNonEmpty(d.Detail, d.Use))
}

func cutNonPassDoors(doors []Door, limit int) int {
	n := 0
	for _, d := range doors {
		if d.Status != StatusPass && len(nonPassLine(d)) > limit {
			n++
		}
	}
	return n
}

// renderDoorLines sizes PASS recipes as if every non-PASS line were capped at
// badLineBytes; badExtra lets non-PASS lines grow past that cap afterwards.
func renderDoorLines(doors []Door, budget, badExtra int) []string {
	if len(doors) == 0 {
		return []string{"doors: none observed (no door report yet)"}
	}
	var bad, good []Door
	for _, d := range doors {
		if d.Status == StatusPass {
			good = append(good, d)
		} else {
			bad = append(bad, d)
		}
	}
	var lines []string
	used := 0
	for _, d := range bad {
		used += len(truncBytes(nonPassLine(d), badLineBytes)) + 1
		lines = append(lines, truncBytes(nonPassLine(d), badLineBytes+badExtra))
	}
	if len(good) == 0 {
		return lines
	}
	room := budget - used
	// Every recipe whole when they all fit: an equal share would cut the long
	// ones while short ones leave bytes unused.
	whole := make([]string, 0, len(good))
	wholeBytes := 0
	for _, d := range good {
		whole = append(whole, "PASS "+d.Name+": "+d.Use)
		wholeBytes += len(whole[len(whole)-1]) + 1
	}
	if wholeBytes <= room {
		return append(lines, whole...)
	}
	per := 0
	if room > 0 {
		per = room/len(good) - 1
	}
	if per < 26 {
		names := make([]string, 0, len(good))
		for _, d := range good {
			names = append(names, d.Name)
		}
		return append(lines, truncBytes("PASS: "+strings.Join(names, " ")+" (recipes: compound doors show)", maxInt(room, 40)))
	}
	if per > maxRecipeBytes {
		per = maxRecipeBytes
	}
	for _, d := range good {
		lines = append(lines, truncBytes("PASS "+d.Name+": "+d.Use, per))
	}
	return lines
}

func renderMCPLine(sets []MCPSet, budget int) string {
	if len(sets) == 0 {
		return ""
	}
	// Per-harness share keeps a 23-server bridge from crowding out the rest.
	share := budget / len(sets)
	parts := make([]string, 0, len(sets))
	for _, s := range sets {
		parts = append(parts, fmt.Sprintf("%s[%s]", s.Harness, joinCapped(s.Servers, share-len(s.Harness)-3)))
	}
	return truncBytes("MCP: "+strings.Join(parts, " "), budget)
}

func renderModelLine(ms []ModelLane, budget int) string {
	if len(ms) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, strings.TrimPrefix(m.Launcher, "claude-")+"="+m.Route)
	}
	return "Models: " + joinCapped(parts, budget-8)
}

func renderChangedLine(cs []Change, now time.Time, budget int) string {
	if len(cs) == 0 {
		return ""
	}
	sorted := append([]Change(nil), cs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At > sorted[j].At })
	parts := make([]string, 0, len(sorted))
	for _, c := range sorted {
		age := ""
		if t, err := time.Parse(time.RFC3339, c.At); err == nil {
			age = " (" + humanAge(now.Sub(t)) + ")"
		}
		parts = append(parts, c.What+age)
	}
	return "Changed: " + joinCapped(parts, budget-9)
}

// joinCapped joins items with "," until the budget, then states how many were left out.
func joinCapped(items []string, budget int) string {
	if budget < 12 {
		budget = 12
	}
	var b strings.Builder
	for i, it := range items {
		sep := ""
		if i > 0 {
			sep = ","
		}
		tail := fmt.Sprintf(",+%d", len(items)-i)
		if b.Len()+len(sep)+len(it) > budget-len(tail) && i > 0 {
			b.WriteString(fmt.Sprintf(",+%d more", len(items)-i))
			return b.String()
		}
		if i == 0 && len(it) > budget {
			return truncBytes(it, budget)
		}
		b.WriteString(sep + it)
	}
	return b.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
