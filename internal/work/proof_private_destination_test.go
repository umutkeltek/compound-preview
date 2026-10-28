package work

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestPrivateSourceDestinationAuthenticatesActualSinglePushURL(t *testing.T) {
	delivery := envelope.GitDeliveryContract{PublicationMode: "required", TargetLocalRef: "refs/heads/main", PublicationRef: "refs/remotes/origin/main"}
	ssh := url.URL{Scheme: "ssh", User: url.User("git"), Host: "github.com", Path: "/example-owner/proof-fixture.git"}
	scp := ssh.User.Username() + "@" + ssh.Host + ":" + strings.TrimPrefix(ssh.Path, "/")
	for _, transport := range []string{"https://github.com/example-owner/proof-fixture.git", scp, ssh.String()} {
		calls := []string{}
		command := func(_ context.Context, _, name string, args ...string) ([]byte, error) {
			key := name + " " + strings.Join(args, " ")
			calls = append(calls, key)
			switch key {
			case "git remote get-url --push --all origin":
				return []byte(transport + "\n"), nil
			case "gh api --hostname github.com user":
				return []byte(`{"id":7,"login":"synthetic-reviewer"}`), nil
			case "gh api --hostname github.com repos/example-owner/proof-fixture":
				return []byte(`{"id":11,"full_name":"example-owner/proof-fixture","private":true,"visibility":"private","archived":false,"permissions":{"push":true}}`), nil
			default:
				return nil, fmt.Errorf("unexpected command")
			}
		}
		got, err := observePrivateSourceDestination(context.Background(), t.TempDir(), delivery, time.Now(), command)
		if err != nil || got.RepositoryID != 11 || got.ViewerID != 7 || got.PushURL != transport || got.CanonicalURL != "https://github.com/example-owner/proof-fixture.git" || len(calls) != 3 {
			t.Fatalf("authenticated destination mismatch: %+v %v calls=%v", got, err, calls)
		}
	}
}

func TestPrivateSourceDestinationRefusesAmbiguousUnknownAndChangedIdentity(t *testing.T) {
	for _, change := range []string{"multiple-push", "wrong-host", "credential-url", "http", "port", "query", "encoded-path", "missing-private", "missing-archived", "public", "internal", "archived", "no-push", "renamed", "viewer", "offline", "wrong-ref"} {
		t.Run(change, func(t *testing.T) {
			delivery := envelope.GitDeliveryContract{PublicationMode: "required", TargetLocalRef: "refs/heads/main", PublicationRef: "refs/remotes/origin/main"}
			if change == "wrong-ref" {
				delivery.PublicationRef = "refs/remotes/origin/other"
			}
			command := func(_ context.Context, _, name string, args ...string) ([]byte, error) {
				if change == "offline" {
					return nil, fmt.Errorf("offline")
				}
				if name == "git" {
					value := "https://github.com/example-owner/proof-fixture.git"
					switch change {
					case "multiple-push":
						value += "\nhttps://github.com/example-owner/other.git"
					case "wrong-host":
						value = "https://example.invalid/example-owner/proof-fixture.git"
					case "credential-url":
						value = "https://" + "placeholder_user:placeholder_value@" + "github.com/example-owner/proof-fixture.git"
					case "http":
						value = strings.Replace(value, "https:", "http:", 1)
					case "port":
						value = strings.Replace(value, "github.com", "github.com:443", 1)
					case "query":
						value += "?unbound=1"
					case "encoded-path":
						value = strings.Replace(value, "example-owner", "%65xample-owner", 1)
					}
					return []byte(value), nil
				}
				if args[len(args)-1] == "user" {
					if change == "viewer" {
						return []byte(`{"id":0,"login":"synthetic-reviewer"}`), nil
					}
					return []byte(`{"id":7,"login":"synthetic-reviewer"}`), nil
				}
				repository := map[string]any{"id": 11, "full_name": "example-owner/proof-fixture", "private": true, "visibility": "private", "archived": false, "permissions": map[string]bool{"push": true}}
				switch change {
				case "missing-private":
					delete(repository, "private")
				case "missing-archived":
					delete(repository, "archived")
				case "public":
					repository["private"], repository["visibility"] = false, "public"
				case "internal":
					repository["visibility"] = "internal"
				case "archived":
					repository["archived"] = true
				case "no-push":
					repository["permissions"] = map[string]bool{"push": false}
				case "renamed":
					repository["full_name"] = "example-owner/renamed-fixture"
				}
				return json.Marshal(repository)
			}
			if _, err := observePrivateSourceDestination(context.Background(), t.TempDir(), delivery, time.Now(), command); err == nil {
				t.Fatal("unknown/ambiguous/private identity refusal was bypassed")
			}
		})
	}
}

func TestPrivateSourceSCPTransportRejectsNonGitHubAuthority(t *testing.T) {
	for _, authority := range []struct{ user, host string }{
		{"operator", "github.com"}, {"git", "example.invalid"},
		{"git", "github.com/other"}, {"git", "github.com:443"},
	} {
		transport := authority.user + "@" + authority.host + ":example-owner/proof-fixture.git"
		if _, _, err := privateGitHubURL(transport); err == nil {
			t.Fatalf("non-GitHub SCP authority accepted: user=%s host=%s", authority.user, authority.host)
		}
	}
}
