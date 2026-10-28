package contract

// Static clients, executor presets and ContextPacks share task-free guidance.
// The orient entry rule stays separate from communication guidance.

const humanOutputBaseline = `Help the person understand and decide. Lead with the useful conclusion, change, or decision—not the machinery. Use clear, concrete, active language, roughly 80% of the way toward simplified technical English, without claiming formal STE compliance. Keep natural rhythm, warmth, personality, and creative choices; do not impose a rigid response template. Separate observed facts from uncertainty, interpretation, and proposed action. Choose the smallest useful medium: prose for simple answers, diagrams for relationships, interactive HTML for exploration, and bespoke explainers or video when motion or time materially aids understanding. Explain what evidence means first and keep exact citations, identifiers, and technical detail available in drilldown. Do not invent evidence, hide gaps, weaken governance, or spend/publish beyond the task's authority.`

const humanOutputOrientReminder = "Begin non-trivial tasks with `compound orient --harness <supported-name> --cwd <repository> --task <task>` and use the owned references from the served context."

// The door pointer rides only on static surfaces (tracked AGENTS.md/CLAUDE.md,
// executor presets). Hook-served harnesses receive the door map itself at
// session start, so the ContextPack summary stays inside its hook budget.
// Every command it names ships in this binary, so a fresh public clone can run
// each one; private recipe collections are reached through the served map.
const humanOutputDoorPointer = "Before acting on the machine (browser, GUI, simulator, App Store, web research), run `compound doors serve` (door map: one line per door, with its command); `compound doors show` (full recipes); `compound doors refresh` (re-check doors)."

func HumanOutputBaseline() string       { return humanOutputBaseline }
func HumanOutputOrientReminder() string { return humanOutputOrientReminder }
func HumanOutputDoorPointer() string    { return humanOutputDoorPointer }
