package work

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const ProofAdoptionUsage = "usage: compound work proof-review WORK --actor ACTOR --file REVIEW [--expected-policy ID --expected-action ID]\n   or: compound work proof-manifest WORK --actor ACTOR --file MANIFEST.json\n   or: compound work proof-adopt WORK --actor ACTOR --proof-result pfrs_ID --manifest artifact:sha256:DIGEST"

type proofAdoptionCLIOptions struct{ work, actor, file, result, manifest, expectedPolicy, expectedAction string }

func parseProofAdoptionCLI(operation string, args []string) (proofAdoptionCLIOptions, error) {
	var opts proofAdoptionCLIOptions
	if len(args) == 0 || !validProofWorkID(args[0]) {
		return opts, fmt.Errorf("work id is required first")
	}
	opts.work = args[0]
	flags := flag.NewFlagSet(operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.actor, "actor", "", "current holder")
	if operation == "proof-manifest" || operation == "proof-review" {
		flags.StringVar(&opts.file, "file", "", "review or manifest body")
	} else if operation == "proof-adopt" {
		flags.StringVar(&opts.result, "proof-result", "", "original proof result")
		flags.StringVar(&opts.manifest, "manifest", "", "custodied manifest")
	} else {
		return opts, fmt.Errorf("unknown proof operation")
	}
	if operation == "proof-review" {
		flags.StringVar(&opts.expectedPolicy, "expected-policy", "", "exact current policy generation")
		flags.StringVar(&opts.expectedAction, "expected-action", "", "exact current action generation")
	}
	flags.Bool("json", false, "structured output")
	seen := map[string]bool{}
	for _, arg := range args[1:] {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		key := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
		if seen[key] {
			return opts, fmt.Errorf("duplicate flag %s", key)
		}
		seen[key] = true
	}
	if err := flags.Parse(args[1:]); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 || strings.TrimSpace(opts.actor) == "" {
		return opts, fmt.Errorf("exact actor and registered flags are required")
	}
	if (operation == "proof-manifest" || operation == "proof-review") && opts.file == "" {
		return opts, fmt.Errorf("proof body file is required")
	}
	if operation == "proof-adopt" {
		if _, err := proofAdoptionRef(opts.result, opts.manifest); err != nil {
			return opts, err
		}
	}
	return opts, nil
}

func ProofAdoptionCLIRequiresWritableStore(operation string, args []string) bool {
	_, err := parseProofAdoptionCLI(operation, args)
	return err == nil
}

func RunProofAdoptionCLI(store *Store, operation string, args []string, stdout, stderr io.Writer) int {
	opts, err := parseProofAdoptionCLI(operation, args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, ProofAdoptionUsage)
		return 2
	}
	var out any
	if operation == "proof-manifest" || operation == "proof-review" {
		info, statErr := os.Lstat(opts.file)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			fmt.Fprintln(stderr, "proof body must be a bounded regular file")
			return 2
		}
		file, openErr := os.Open(opts.file)
		if openErr != nil {
			fmt.Fprintln(stderr, openErr)
			return 1
		}
		body, readErr := io.ReadAll(io.LimitReader(file, 1<<20+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(body) > 1<<20 {
			fmt.Fprintln(stderr, "invalid bounded proof body")
			return 2
		}
		if operation == "proof-review" {
			out, err = store.RecordProofReview(opts.work, ProofReviewInput{
				Actor: opts.actor, Body: body, ExpectedPolicyDecisionID: opts.expectedPolicy,
				ExpectedActionIntentID: opts.expectedAction,
			}, time.Now())
		} else {
			var manifest ProofMemberManifest
			if publicReviewStrictJSON(body, &manifest) != nil {
				fmt.Fprintln(stderr, "invalid proof manifest")
				return 2
			}
			out, err = store.RecordProofManifest(opts.work, opts.actor, manifest, time.Now())
		}
	} else {
		out, err = store.AdoptProof(opts.work, ProofAdoptionInput{Actor: opts.actor, ProofResultID: opts.result, ManifestRef: opts.manifest}, time.Now())
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
