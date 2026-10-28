package work

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
)

// PrivateSourceDestination is an authenticated observation, not a declaration
// that a repository name or a WorkEnvelope's privacy field implies privacy.
// Credential bytes and complete API responses never enter this evidence.
type PrivateSourceDestination struct {
	Host           string `json:"host"`
	RepositoryID   int64  `json:"repository_id"`
	FullName       string `json:"full_name"`
	CanonicalURL   string `json:"canonical_url"`
	PushURL        string `json:"push_url"`
	Remote         string `json:"remote"`
	PublicationRef string `json:"publication_ref"`
	TargetRef      string `json:"target_ref"`
	ViewerID       int64  `json:"viewer_id"`
	ViewerLogin    string `json:"viewer_login"`
	Visibility     string `json:"visibility"`
	ObservedAt     string `json:"observed_at"`
}

var privateSourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var privateSourceRemote = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func privateGitHubURL(raw string) (fullName, canonical string, err error) {
	value := raw
	if !strings.Contains(value, "://") {
		authority, repositoryPath, scp := strings.Cut(value, ":")
		if scp {
			user, host, hasUser := strings.Cut(authority, "@")
			if !hasUser || user != "git" || host != "github.com" {
				return "", "", fmt.Errorf("private source SCP destination requires the GitHub Git user and host")
			}
			value = (&url.URL{Scheme: "ssh", User: url.User(user), Host: host, Path: "/" + repositoryPath}).String()
		}
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() != "github.com" || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", "", fmt.Errorf("private source destination must be one literal GitHub URL")
	}
	if (u.Scheme == "https" && u.User != nil) || (u.Scheme == "ssh" && (u.User == nil || u.User.Username() != "git" || u.User.String() != "git")) || (u.Scheme != "https" && u.Scheme != "ssh") {
		return "", "", fmt.Errorf("private source destination contains credentials or unsupported transport")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("private source destination is not owner/repository")
	}
	parts[1] = strings.TrimSuffix(parts[1], ".git")
	if !privateSourceName.MatchString(parts[0]) || !privateSourceName.MatchString(parts[1]) || parts[0] == "." || parts[1] == "." || parts[0] == ".." || parts[1] == ".." {
		return "", "", fmt.Errorf("private source destination name is invalid")
	}
	fullName = strings.ToLower(strings.Join(parts, "/"))
	return fullName, "https://github.com/" + fullName + ".git", nil
}

type privateSourceCommand func(context.Context, string, string, ...string) ([]byte, error)

func runPrivateSourceCommand(ctx context.Context, cwd, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = gitrepository.ReadEnvironment()
	var output proofRedactionBoundedOutput
	output.Limit = 4 << 20
	cmd.Stdout = &output
	// stderr can contain remote transport diagnostics; never copy it into a
	// review, a receipt, or an error containing credentials from local config.
	if err := cmd.Run(); err != nil || output.Exceeded {
		return nil, fmt.Errorf("private source authenticated observation failed")
	}
	return output.Bytes(), nil
}

func observePrivateSourceDestination(ctx context.Context, repo string, delivery envelope.GitDeliveryContract, now time.Time, command privateSourceCommand) (PrivateSourceDestination, error) {
	var out PrivateSourceDestination
	branch := strings.TrimPrefix(delivery.TargetLocalRef, "refs/heads/")
	ref := strings.TrimPrefix(delivery.PublicationRef, "refs/remotes/")
	parts := strings.SplitN(ref, "/", 2)
	if delivery.PublicationMode != "required" || branch == delivery.TargetLocalRef || len(parts) != 2 || !privateSourceRemote.MatchString(parts[0]) || parts[1] != branch {
		return out, fmt.Errorf("private source publication needs one bound remote-tracking branch")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	body, err := command(ctx, repo, "git", "remote", "get-url", "--push", "--all", parts[0])
	if err != nil {
		return out, err
	}
	urls := strings.Fields(strings.TrimSpace(string(body)))
	if len(urls) != 1 {
		return out, fmt.Errorf("private source publication needs exactly one actual push URL")
	}
	fullName, canonical, err := privateGitHubURL(urls[0])
	if err != nil {
		return out, err
	}
	viewerBody, err := command(ctx, repo, "gh", "api", "--hostname", "github.com", "user")
	if err != nil {
		return out, err
	}
	var viewer struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if json.Unmarshal(viewerBody, &viewer) != nil || viewer.ID <= 0 || !privateSourceName.MatchString(viewer.Login) {
		return out, fmt.Errorf("private source authenticated viewer is unavailable")
	}
	repositoryBody, err := command(ctx, repo, "gh", "api", "--hostname", "github.com", "repos/"+fullName)
	if err != nil {
		return out, err
	}
	var repository struct {
		ID          int64  `json:"id"`
		FullName    string `json:"full_name"`
		Private     *bool  `json:"private"`
		Visibility  string `json:"visibility"`
		Archived    *bool  `json:"archived"`
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if json.Unmarshal(repositoryBody, &repository) != nil || repository.ID <= 0 || !strings.EqualFold(repository.FullName, fullName) || repository.Private == nil || !*repository.Private || repository.Visibility != "private" || repository.Archived == nil || *repository.Archived || !repository.Permissions.Push {
		return out, fmt.Errorf("authenticated destination is not the exact writable PRIVATE repository")
	}
	out = PrivateSourceDestination{Host: "github.com", RepositoryID: repository.ID, FullName: fullName,
		CanonicalURL: canonical, PushURL: urls[0], Remote: parts[0], PublicationRef: delivery.PublicationRef,
		TargetRef: delivery.TargetLocalRef, ViewerID: viewer.ID, ViewerLogin: viewer.Login,
		Visibility: "private", ObservedAt: now.UTC().Format(time.RFC3339Nano)}
	return out, nil
}

func samePrivateSourceDestination(expected, observed PrivateSourceDestination) bool {
	expected.ObservedAt, observed.ObservedAt = "", ""
	return expected == observed
}

func validatePrivateSourceDestination(value PrivateSourceDestination, now time.Time) error {
	name, canonical, err := privateGitHubURL(value.PushURL)
	at, timeErr := time.Parse(time.RFC3339Nano, value.ObservedAt)
	if err != nil || timeErr != nil || at.After(now) || !strings.HasSuffix(value.ObservedAt, "Z") || value.Host != "github.com" || value.RepositoryID <= 0 || value.ViewerID <= 0 || !privateSourceName.MatchString(value.ViewerLogin) || value.FullName != name || value.CanonicalURL != canonical || value.Visibility != "private" || !privateSourceRemote.MatchString(value.Remote) || value.PublicationRef != "refs/remotes/"+value.Remote+"/"+strings.TrimPrefix(value.TargetRef, "refs/heads/") || !strings.HasPrefix(value.TargetRef, "refs/heads/") {
		return fmt.Errorf("invalid authenticated private source destination binding")
	}
	return nil
}
