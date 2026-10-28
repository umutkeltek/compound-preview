package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/work"
)

func runProofCheckSealedSnapshotContext(
	ctx context.Context,
	repoPath string,
	dryRun bool,
	source gitrepository.Snapshot,
	ciOnly bool,
	ciConsole bool,
	now time.Time,
) (proofCheckReport, error) {
	originalRepo, err := resolveProofCheckRepo(repoPath)
	if err != nil {
		return proofCheckReport{}, err
	}
	ctx = context.WithValue(ctx, proofCheckCacheOriginKey{}, originalRepo)
	sealedRepo, cleanup, err := prepareProofCheckSealedWorkspace(ctx, originalRepo, source)
	if err != nil {
		return proofCheckReport{}, err
	}

	report, runErr := runProofCheckSnapshotContext(
		ctx,
		sealedRepo,
		dryRun,
		source,
		ciOnly,
		ciConsole,
		now,
	)
	if cleanupErr := cleanup(); cleanupErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("remove sealed proof workspace: %w", cleanupErr))
	}
	report.Repo = originalRepo
	return report, runErr
}

func prepareProofCheckSealedWorkspace(
	ctx context.Context,
	repo string,
	source gitrepository.Snapshot,
) (string, func() error, error) {
	tmpRoot, err := os.MkdirTemp("", "compound-proofcheck-sealed-*")
	if err != nil {
		return "", nil, fmt.Errorf("create sealed proof workspace: %w", err)
	}
	cleanup := func() error {
		return os.RemoveAll(tmpRoot)
	}
	sealedRepo := filepath.Join(tmpRoot, "repo")
	if output, cloneErr := proofCheckCommandContext(
		ctx,
		"git",
		"clone",
		"--quiet",
		"--shared",
		"--no-checkout",
		"--",
		repo,
		sealedRepo,
	).CombinedOutput(); cloneErr != nil {
		cleanupErr := cleanup()
		return "", nil, fmt.Errorf(
			"clone sealed proof workspace: %w",
			errors.Join(
				fmt.Errorf("%w: %s", cloneErr, strings.TrimSpace(string(output))),
				cleanupErr,
			),
		)
	}
	if output, checkoutErr := proofCheckCommandContext(
		ctx,
		"git",
		"-C",
		sealedRepo,
		"-c",
		"advice.detachedHead=false",
		"checkout",
		"--quiet",
		"--detach",
		"--force",
		source.HeadSHA,
	).CombinedOutput(); checkoutErr != nil {
		cleanupErr := cleanup()
		return "", nil, fmt.Errorf(
			"checkout sealed proof head %s: %w",
			source.HeadSHA,
			errors.Join(
				fmt.Errorf("%w: %s", checkoutErr, strings.TrimSpace(string(output))),
				cleanupErr,
			),
		)
	}
	if err := work.CopyProofCeremonyInputs(ctx, repo, sealedRepo, proofCheckCommandContext); err != nil {
		cleanupErr := cleanup()
		return "", nil, errors.Join(err, cleanupErr)
	}
	return sealedRepo, cleanup, nil
}
