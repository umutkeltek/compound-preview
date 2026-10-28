package candidates

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type writerCASPayload struct {
	ID     string `json:"id"`
	Marker string `json:"marker"`
}

func writerCASBody(t *testing.T, payload writerCASPayload) []byte {
	t.Helper()
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(body, '\n')
}

func writerCASTarget(t *testing.T, homePath, id string, body []byte) string {
	t.Helper()
	target := filepath.Join(homePath, InboxDir(Memory), id+".json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return target
}

func assertWriterCASCorruption(t *testing.T, err error, code CorruptionCode) {
	t.Helper()
	var corruption *Corruption
	if !errors.As(err, &corruption) {
		t.Fatalf("error = %v, want typed candidate corruption", err)
	}
	if corruption.Code != code {
		t.Fatalf("corruption code = %q, want %q: %v", corruption.Code, code, err)
	}
}

func assertWriterCASBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("candidate bytes = %q, want %q", got, want)
	}
}

func TestCandidateWritersRejectCorruptExistingTargetsUnchanged(t *testing.T) {
	const id = "writer-corrupt"
	replacement := writerCASPayload{ID: id, Marker: "replacement"}
	cases := []struct {
		name string
		body []byte
		code CorruptionCode
	}{
		{name: "invalid json", body: []byte("{not-json\n"), code: CorruptionInvalidJSON},
		{name: "blank id", body: []byte("{\"id\":\"\",\"marker\":\"blank\"}\n"), code: CorruptionBlankID},
		{name: "filename body mismatch", body: []byte("{\"id\":\"other\",\"marker\":\"alias\"}\n"), code: CorruptionIDMismatch},
	}
	writers := []struct {
		name string
		run  func(string) (bool, error)
	}{
		{name: "WriteCandidate", run: func(homePath string) (bool, error) {
			return false, WriteCandidate(homePath, Memory, id, replacement)
		}},
		{name: "CreateCandidateIfAbsent", run: func(homePath string) (bool, error) {
			return CreateCandidateIfAbsent(homePath, Memory, id, replacement)
		}},
	}
	for _, writer := range writers {
		for _, tc := range cases {
			t.Run(writer.name+"/"+tc.name, func(t *testing.T) {
				homePath := t.TempDir()
				target := writerCASTarget(t, homePath, id, tc.body)
				created, err := writer.run(homePath)
				if err == nil {
					t.Fatalf("%s accepted corrupt existing target", writer.name)
				}
				if created {
					t.Fatalf("%s reported corrupt target created", writer.name)
				}
				assertWriterCASCorruption(t, err, tc.code)
				assertWriterCASBytes(t, target, tc.body)
			})
		}
	}
}

func TestWriteCandidateRejectsTargetDriftAndRetriesExactCurrentState(t *testing.T) {
	const id = "writer-target-drift"
	original := writerCASBody(t, writerCASPayload{ID: id, Marker: "original"})
	racer := writerCASBody(t, writerCASPayload{ID: id, Marker: "racer"})
	replacement := writerCASPayload{ID: id, Marker: "replacement"}
	want := writerCASBody(t, replacement)

	for _, tc := range []struct {
		name    string
		replace bool
	}{
		{name: "removed after snapshot"},
		{name: "replaced after snapshot", replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			target := writerCASTarget(t, homePath, id, original)
			held := target + ".held"
			realPublish := publishCandidateReplacement
			inject := true
			publishCandidateReplacement = func(
				directory *candidateDirectory,
				targetName string,
				tempName string,
				expected *Record,
				staged Record,
			) error {
				if inject {
					inject = false
					if err := os.Rename(directory.path(targetName), held); err != nil {
						t.Fatal(err)
					}
					if tc.replace {
						if err := os.WriteFile(directory.path(targetName), racer, 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
				return realPublish(directory, targetName, tempName, expected, staged)
			}
			t.Cleanup(func() {
				publishCandidateReplacement = realPublish
				if held != "" {
					_ = os.Remove(held)
				}
			})

			err := WriteCandidate(homePath, Memory, id, replacement)
			assertWriterCASCorruption(t, err, CorruptionRaced)
			assertWriterCASBytes(t, held, original)
			if tc.replace {
				assertWriterCASBytes(t, target, racer)
			} else {
				if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
					t.Fatalf("writer published after target removal: %v", statErr)
				}
				if err := os.Rename(held, target); err != nil {
					t.Fatal(err)
				}
				held = ""
			}

			if err := WriteCandidate(homePath, Memory, id, replacement); err != nil {
				t.Fatalf("stable retry: %v", err)
			}
			assertWriterCASBytes(t, target, want)
		})
	}
}

func TestCandidateWritersRejectStagedTempSwapBeforePublish(t *testing.T) {
	const id = "writer-staged-drift"
	original := writerCASBody(t, writerCASPayload{ID: id, Marker: "original"})
	replacement := writerCASPayload{ID: id, Marker: "replacement"}
	want := writerCASBody(t, replacement)
	attacker := writerCASBody(t, writerCASPayload{ID: id, Marker: "staged-racer"})

	t.Run("WriteCandidate", func(t *testing.T) {
		homePath := t.TempDir()
		target := writerCASTarget(t, homePath, id, original)
		realPublish := publishCandidateReplacement
		inject := true
		publishCandidateReplacement = func(
			directory *candidateDirectory,
			targetName string,
			tempName string,
			expected *Record,
			staged Record,
		) error {
			heldName := ""
			if inject {
				inject = false
				heldName = tempName + ".held"
				if err := directory.rename(tempName, heldName); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(directory.path(tempName), attacker, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := realPublish(directory, targetName, tempName, expected, staged)
			if heldName != "" {
				if removeErr := directory.remove(heldName); removeErr != nil {
					t.Fatal(removeErr)
				}
			}
			return err
		}
		t.Cleanup(func() { publishCandidateReplacement = realPublish })

		err := WriteCandidate(homePath, Memory, id, replacement)
		assertWriterCASCorruption(t, err, CorruptionRaced)
		assertWriterCASBytes(t, target, original)
		if err := WriteCandidate(homePath, Memory, id, replacement); err != nil {
			t.Fatalf("stable retry: %v", err)
		}
		assertWriterCASBytes(t, target, want)
	})

	t.Run("CreateCandidateIfAbsent", func(t *testing.T) {
		homePath := t.TempDir()
		target := filepath.Join(homePath, InboxDir(Memory), id+".json")
		realPublish := publishCandidateCreation
		inject := true
		publishCandidateCreation = func(
			directory *candidateDirectory,
			targetName string,
			tempName string,
			staged Record,
		) error {
			heldName := ""
			if inject {
				inject = false
				heldName = tempName + ".held"
				if err := directory.rename(tempName, heldName); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(directory.path(tempName), attacker, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := realPublish(directory, targetName, tempName, staged)
			if heldName != "" {
				if removeErr := directory.remove(heldName); removeErr != nil {
					t.Fatal(removeErr)
				}
			}
			return err
		}
		t.Cleanup(func() { publishCandidateCreation = realPublish })

		created, err := CreateCandidateIfAbsent(homePath, Memory, id, replacement)
		if created {
			t.Fatal("staged-temp race reported created")
		}
		assertWriterCASCorruption(t, err, CorruptionRaced)
		if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
			t.Fatalf("staged-temp race published target: %v", statErr)
		}
		created, err = CreateCandidateIfAbsent(homePath, Memory, id, replacement)
		if err != nil || !created {
			t.Fatalf("stable retry created=%v err=%v", created, err)
		}
		assertWriterCASBytes(t, target, want)
	})
}

func TestCandidateWritersValidatePublishedPoststate(t *testing.T) {
	const id = "writer-poststate"
	original := writerCASBody(t, writerCASPayload{ID: id, Marker: "original"})
	replacement := writerCASPayload{ID: id, Marker: "replacement"}
	want := writerCASBody(t, replacement)
	attacker := writerCASBody(t, writerCASPayload{ID: id, Marker: "post-publish-racer"})

	t.Run("WriteCandidate", func(t *testing.T) {
		homePath := t.TempDir()
		target := writerCASTarget(t, homePath, id, original)
		realRename := renameCandidateFile
		inject := true
		renameCandidateFile = func(directory *candidateDirectory, oldName, newName string) error {
			if err := realRename(directory, oldName, newName); err != nil {
				return err
			}
			if inject {
				inject = false
				heldName := newName + ".published-held"
				if err := directory.rename(newName, heldName); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(directory.path(newName), attacker, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := directory.remove(heldName); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}
		t.Cleanup(func() { renameCandidateFile = realRename })

		err := WriteCandidate(homePath, Memory, id, replacement)
		assertWriterCASCorruption(t, err, CorruptionRaced)
		assertWriterCASBytes(t, target, attacker)
		if err := WriteCandidate(homePath, Memory, id, replacement); err != nil {
			t.Fatalf("stable recovery retry: %v", err)
		}
		assertWriterCASBytes(t, target, want)
	})

	t.Run("CreateCandidateIfAbsent", func(t *testing.T) {
		homePath := t.TempDir()
		target := filepath.Join(homePath, InboxDir(Memory), id+".json")
		realLink := linkCandidateFile
		inject := true
		linkCandidateFile = func(directory *candidateDirectory, oldName, newName string) error {
			if err := realLink(directory, oldName, newName); err != nil {
				return err
			}
			if inject {
				inject = false
				heldName := newName + ".published-held"
				if err := directory.rename(newName, heldName); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(directory.path(newName), attacker, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := directory.remove(heldName); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		}
		t.Cleanup(func() { linkCandidateFile = realLink })

		created, err := CreateCandidateIfAbsent(homePath, Memory, id, replacement)
		if created {
			t.Fatal("invalid poststate reported created")
		}
		assertWriterCASCorruption(t, err, CorruptionRaced)
		assertWriterCASBytes(t, target, attacker)
		created, err = CreateCandidateIfAbsent(homePath, Memory, id, replacement)
		if err != nil || created {
			t.Fatalf("retry against exact existing state created=%v err=%v", created, err)
		}
		assertWriterCASBytes(t, target, attacker)
	})
}
