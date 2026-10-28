package work

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const PrivateSourcePublishUsage = "usage: compound work private-source-publish --work WORK --actor ACTOR --repo CLEAN_SOURCE --review-action ACT [--json]\n   or: compound work private-source-publish --release-tag vMAJOR.MINOR.PATCH --request COMPLETED_PUBLICATION [--json]\n   or: compound work private-source-publish --apply --request FILE [--json]"

type privateSourcePublishCLIOptions struct {
	work, actor, repo, review, request, releaseTag string
	apply                                          bool
}

func parsePrivateSourcePublishCLI(args []string) (privateSourcePublishCLIOptions, error) {
	var opts privateSourcePublishCLIOptions
	flags := flag.NewFlagSet("private-source-publish", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.work, "work", "", "bound work")
	flags.StringVar(&opts.actor, "actor", "", "lease holder")
	flags.StringVar(&opts.repo, "repo", "", "clean certified source workspace")
	flags.StringVar(&opts.review, "review-action", "", "exact native private review")
	flags.StringVar(&opts.request, "request", "", "reviewed publication request")
	flags.StringVar(&opts.releaseTag, "release-tag", "", "plan separate immutable release tag for a completed publication")
	flags.BoolVar(&opts.apply, "apply", false, "perform governed exact publication")
	flags.Bool("json", false, "structured result")
	seen := map[string]bool{}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
		if seen[name] {
			return opts, fmt.Errorf("duplicate flag %s", name)
		}
		seen[name] = true
	}
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("unexpected publication argument")
	}
	if opts.apply {
		if opts.request == "" || opts.work != "" || opts.actor != "" || opts.repo != "" || opts.review != "" || opts.releaseTag != "" {
			return opts, fmt.Errorf("apply requires only a concrete request")
		}
	} else if opts.releaseTag != "" {
		if opts.request == "" || opts.work != "" || opts.actor != "" || opts.repo != "" || opts.review != "" || len(opts.releaseTag) > 64 || !privateReleaseVersion.MatchString(opts.releaseTag) {
			return opts, fmt.Errorf("release tag planning requires only a completed publication request and numeric version")
		}
	} else if opts.request != "" || opts.work == "" || opts.actor == "" || opts.repo == "" || opts.review == "" {
		return opts, fmt.Errorf("planning requires work, actor, clean source, and exact review")
	}
	return opts, nil
}

func PrivateSourcePublishCLIRequiresWritableStore(args []string) bool {
	opts, err := parsePrivateSourcePublishCLI(args)
	return err == nil && opts.apply
}

func RunPrivateSourcePublishCLI(store *Store, args []string, stdout, stderr io.Writer) int {
	opts, err := parsePrivateSourcePublishCLI(args)
	if err != nil {
		fmt.Fprintln(stderr, PrivateSourcePublishUsage, "\nerror:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var result any
	if opts.apply || opts.releaseTag != "" {
		file, openErr := os.Open(opts.request)
		if openErr != nil {
			fmt.Fprintln(stderr, "error: publication request unavailable")
			return 1
		}
		body, readErr := io.ReadAll(io.LimitReader(file, 1<<20+1))
		closeErr := file.Close()
		var identity struct {
			SchemaVersion string `json:"schema_version"`
		}
		if readErr != nil || closeErr != nil || len(body) > 1<<20 || json.Unmarshal(body, &identity) != nil {
			fmt.Fprintln(stderr, "error: invalid bounded publication request")
			return 2
		}
		switch identity.SchemaVersion {
		case PrivateSourcePublishSchema:
			var request PrivateSourcePublishRequest
			if publicReviewStrictJSON(body, &request) != nil {
				fmt.Fprintln(stderr, "error: invalid strict source publication request")
				return 2
			}
			if opts.releaseTag != "" {
				result, err = store.PrivateReleaseTagPlan(ctx, request, opts.releaseTag, time.Now())
			} else {
				result, err = store.ApplyPrivateSourcePublication(ctx, request, time.Now())
			}
		case PrivateReleaseTagSchema:
			var request PrivateReleaseTagRequest
			if !opts.apply || publicReviewStrictJSON(body, &request) != nil {
				fmt.Fprintln(stderr, "error: invalid strict release tag request")
				return 2
			}
			result, err = store.ApplyPrivateReleaseTag(ctx, request, time.Now())
		default:
			fmt.Fprintln(stderr, "error: unsupported publication request schema")
			return 2
		}
	} else {
		result, err = store.PrivateSourcePublishPlan(ctx, opts.work, opts.actor, opts.repo, opts.review, time.Now())
	}
	if result != nil {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if encodeErr := encoder.Encode(result); encodeErr != nil {
			fmt.Fprintln(stderr, "error: publication output unavailable")
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}
