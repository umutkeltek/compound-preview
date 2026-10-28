// Package game is compound's studio-facing operator surface.
//
// The game surface observes game-genre and game-company signals from public
// web APIs (Steam Web API + SteamSpy), emits SourceObservation rows under
// COMPOUND_HOME/observations/, and exposes read-only catalog projections
// (genres/companies/trending) plus a deterministic evidence-packet scout and
// an LLM-rented idea draft that parks product-hypothesis candidates.
//
// Doctrinal anchors (compound's standing law):
//
//   - Adapters observe; they do not promote. Every SourceObservation emitted
//     from this package must have MapsTo[].PromoteAllowed = false.
//   - Evidence not truth. Catalog projections are stateful reads over the
//     SourceObservation store; they never write back.
//   - Human on irreversible decisions. Unity scaffold and release-plan paths
//     emit JSON; neither writes to disk in v0.
package game

const (
	SchemaGameGenres         = "compound/game-genres.v0.1"
	SchemaGameCompanies      = "compound/game-companies.v0.1"
	SchemaGameTrending       = "compound/game-trending.v0.1"
	SchemaGameDoctor         = "compound/game-doctor.v0.1"
	SchemaGameIdea           = "compound/game-idea.v0.1"
	SchemaGameUnityBlueprint = "compound/game-unity-blueprint.v0.1"
	SchemaGameReleasePlan    = "compound/game-release-plan.v0.1"
)
