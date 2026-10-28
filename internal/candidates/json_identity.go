package candidates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type candidateJSONIdentity struct {
	ID   string
	Kind string
}

type candidateIdentityDecodeError struct {
	code   CorruptionCode
	detail string
	cause  error
}

func (err *candidateIdentityDecodeError) Error() string {
	if err == nil {
		return "candidate identity decode error"
	}
	return err.detail
}

func (err *candidateIdentityDecodeError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func decodeCandidateJSONIdentity(data []byte) (candidateJSONIdentity, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return candidateJSONIdentity{}, malformedCandidateIdentity("decode candidate JSON object", err)
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return candidateJSONIdentity{}, malformedCandidateIdentity("candidate JSON must be an object", nil)
	}

	var identity candidateJSONIdentity
	seenID := false
	seenKind := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return candidateJSONIdentity{}, malformedCandidateIdentity("decode candidate JSON key", err)
		}
		key, ok := token.(string)
		if !ok {
			return candidateJSONIdentity{}, malformedCandidateIdentity("candidate JSON object key must be a string", nil)
		}
		switch {
		case key == "id":
			if seenID {
				return candidateJSONIdentity{}, nonCanonicalCandidateIdentity("candidate JSON id key must be unique", nil)
			}
			seenID = true
			if err := decoder.Decode(&identity.ID); err != nil {
				return candidateJSONIdentity{}, nonCanonicalCandidateIdentity("candidate JSON id must be a string", err)
			}
		case strings.EqualFold(key, "id"):
			return candidateJSONIdentity{}, nonCanonicalCandidateIdentity(fmt.Sprintf("candidate JSON identity key %q must be exact lowercase id", key), nil)
		case key == "kind":
			if seenKind {
				return candidateJSONIdentity{}, nonCanonicalCandidateIdentity("candidate JSON kind key must be unique", nil)
			}
			seenKind = true
			if err := decoder.Decode(&identity.Kind); err != nil {
				return candidateJSONIdentity{}, nonCanonicalCandidateIdentity("candidate JSON kind must be a string", err)
			}
		case strings.EqualFold(key, "kind"):
			return candidateJSONIdentity{}, nonCanonicalCandidateIdentity(fmt.Sprintf("candidate JSON kind key %q must be exact lowercase kind", key), nil)
		default:
			var ignored json.RawMessage
			if err := decoder.Decode(&ignored); err != nil {
				return candidateJSONIdentity{}, malformedCandidateIdentity(fmt.Sprintf("decode candidate JSON field %q", key), err)
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return candidateJSONIdentity{}, malformedCandidateIdentity("close candidate JSON object", err)
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return candidateJSONIdentity{}, malformedCandidateIdentity("candidate JSON object did not terminate", nil)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("unexpected trailing JSON value")
		}
		return candidateJSONIdentity{}, malformedCandidateIdentity("candidate JSON has trailing content", err)
	}
	return identity, nil
}

func malformedCandidateIdentity(detail string, cause error) error {
	return &candidateIdentityDecodeError{code: CorruptionInvalidJSON, detail: detail, cause: cause}
}

func nonCanonicalCandidateIdentity(detail string, cause error) error {
	return &candidateIdentityDecodeError{code: CorruptionInvalidID, detail: detail, cause: cause}
}
