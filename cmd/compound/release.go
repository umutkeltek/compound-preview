package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/release"
)

func cmdRelease(args []string) {
	if len(args) == 0 || hasFlag(args, "--help") || hasFlag(args, "-h") {
		fmt.Fprintln(os.Stderr, releaseUsage())
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	switch args[0] {
	case "export-public":
		report, err := runReleaseExportPublic(args[1:], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "release export-public:", err)
			os.Exit(1)
		}
		if hasFlag(args[1:], "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# release export-public %s\n\n", report.Status)
		fmt.Printf("- target: %s\n", report.TargetPath)
		fmt.Printf("- commit: %s\n", report.Commit)
		fmt.Printf("- files: %d\n", report.Files)
		fmt.Printf("- bytes: %d\n", report.Bytes)
	case "export-private-audit":
		report, err := runReleaseExportPrivateAudit(args[1:], time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "release export-private-audit:", err)
			os.Exit(1)
		}
		if hasFlag(args[1:], "--json") {
			printJSON(report)
			return
		}
		fmt.Printf("# release export-private-audit %s\n\n", report.Status)
		fmt.Printf("- target: %s\n", report.TargetPath)
		fmt.Printf("- commit: %s\n", report.Commit)
		fmt.Printf("- files: %d\n", report.Files)
		fmt.Printf("- bytes: %d\n", report.Bytes)
		if len(report.PrivateSurfaces) > 0 {
			fmt.Printf("- private_surfaces: %s\n", strings.Join(report.PrivateSurfaces, ", "))
		}
		if len(report.Warnings) > 0 {
			fmt.Printf("- warnings: %s\n", strings.Join(report.Warnings, "; "))
		}
	default:
		fmt.Fprintln(os.Stderr, releaseUsage())
		os.Exit(2)
	}
}

func runReleaseExportPublic(args []string, now time.Time) (release.PublicExportReport, error) {
	repo := flagValue(args, "--repo")
	target := flagValue(args, "--target")
	if strings.TrimSpace(target) == "" {
		homePath, err := home.Resolve("")
		if err != nil {
			return release.PublicExportReport{}, err
		}
		target = filepath.Join(homePath, "exports", "public-source", "compound-public-"+now.UTC().Format("20060102T150405Z")+".tar.gz")
	}
	return release.ExportPublic(context.Background(), release.PublicExportOptions{
		RepoPath:   repo,
		TargetPath: target,
		Prefix:     flagValue(args, "--prefix"),
	}, now)
}

func runReleaseExportPrivateAudit(args []string, now time.Time) (release.PrivateAuditExportReport, error) {
	repo := flagValue(args, "--repo")
	target := flagValue(args, "--target")
	if strings.TrimSpace(target) == "" {
		homePath, err := home.Resolve("")
		if err != nil {
			return release.PrivateAuditExportReport{}, err
		}
		target = filepath.Join(homePath, "exports", "private-audit", "compound-private-audit-"+now.UTC().Format("20060102T150405Z")+".tar.gz")
	}
	return release.ExportPrivateAudit(context.Background(), release.PrivateAuditExportOptions{
		RepoPath:   repo,
		TargetPath: target,
		Prefix:     flagValue(args, "--prefix"),
	}, now)
}

func releaseUsage() string {
	return "usage: compound release export-public [--repo P] [--target archive.tar.gz] [--prefix NAME] [--json]\n       compound release export-private-audit [--repo P] [--target archive.tar.gz] [--prefix NAME] [--json]"
}
