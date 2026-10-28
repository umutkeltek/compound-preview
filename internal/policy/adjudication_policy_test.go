package policy

import "testing"

func TestResolveAdjudicationDecisionsDown(t *testing.T) {
	cases := []struct {
		name string
		in   AdjudicationInput
		want string
	}{
		{
			name: "operator permanent claim type",
			in:   AdjudicationInput{ClaimType: "decision", Visibility: "operational"},
			want: DispositionOperatorRequired,
		},
		{
			name: "preference is operator permanent",
			in:   AdjudicationInput{ClaimType: "preference", Visibility: "operational"},
			want: DispositionOperatorRequired,
		},
		{
			name: "explicit confirmation",
			in:   AdjudicationInput{PromotionBasis: "explicit_confirmation", Visibility: "operational"},
			want: DispositionOperatorRequired,
		},
		{
			name: "un-evidenced durable",
			in:   AdjudicationInput{Visibility: "operational"},
			want: DispositionDecayPark,
		},
		{
			name: "cheap lane",
			in:   AdjudicationInput{ProvenanceLane: "cheap-lane", Visibility: "operational"},
			want: DispositionDecayPark,
		},
		{
			name: "cheap lane runtime proof decay parks before auto-file",
			in: AdjudicationInput{
				ProvenanceLane: "cheap-lane",
				PromotionBasis: "runtime_proof",
				Visibility:     "operational",
			},
			want: DispositionDecayPark,
		},
		{
			name: "deterministic import auto-files",
			in:   AdjudicationInput{PromotionBasis: "trusted_deterministic_import", Visibility: "operational"},
			want: DispositionAutoFile,
		},
		{
			name: "private never auto-files",
			in:   AdjudicationInput{PromotionBasis: "trusted_deterministic_import", Visibility: "private"},
			want: DispositionOperatorRequired,
		},
		{
			name: "canon class principle permanent",
			in:   AdjudicationInput{CanonClass: "principle", Visibility: "operational"},
			want: DispositionOperatorRequired,
		},
		{
			// Falsifier for the pointer guard: a bare content hash with no basis is a
			// pointer, not a claim — decay_park would strand it (corroboration can never
			// arrive for a cid). Red without the guard, green with it.
			name: "bare content-hash pointer auto-files",
			in: AdjudicationInput{
				Visibility: "operational",
				Object:     "sha256:277a3a18dd61158433163f7a8c42aa0dc47324db88eeb9bc2a3d089fb5f0cb4a",
			},
			want: DispositionAutoFile,
		},
		{
			name: "private pointer still never auto-files (privacy law wins)",
			in: AdjudicationInput{
				Visibility: "private",
				Object:     "sha256:277a3a18dd61158433163f7a8c42aa0dc47324db88eeb9bc2a3d089fb5f0cb4a",
			},
			want: DispositionDecayPark,
		},
		{
			name: "sentence containing a hash is a claim, not a pointer",
			in: AdjudicationInput{
				Visibility: "operational",
				Object:     "build artifact sha256:277a3a18dd61158433163f7a8c42aa0dc47324db88eeb9bc2a3d089fb5f0cb4a verified on CI",
			},
			want: DispositionDecayPark,
		},
		{
			name: "cheap-lane pointer still decay-parks (cheap lane never raises tier)",
			in: AdjudicationInput{
				ProvenanceLane: "cheap-lane",
				Visibility:     "operational",
				Object:         "sha256:277a3a18dd61158433163f7a8c42aa0dc47324db88eeb9bc2a3d089fb5f0cb4a",
			},
			want: DispositionDecayPark,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveAdjudication(tc.in)
			if got.Kind != tc.want {
				t.Fatalf("kind = %q, want %q (reason=%q)", got.Kind, tc.want, got.Reason)
			}
			if got.Kind == DispositionAutoFile && tc.in.Visibility == "private" {
				t.Fatal("private must never auto_file")
			}
			if got.Kind == DispositionDecayPark && got.ParkDays != DefaultDecayParkDays {
				t.Fatalf("park days = %d", got.ParkDays)
			}
		})
	}
}
