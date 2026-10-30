// Package evidence builds, signs and verifies CInema receipts: an in-toto
// Statement in a DSSE envelope, signed with Ed25519 (plan §7). Verification is
// strict and returns a VerifiedStatement, never a decision: only package plan
// turns statements into REUSE/RUN (plan §3.2 type rule).
package evidence

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"
)

const (
	PayloadType   = "application/vnd.in-toto+json"
	StatementType = "https://in-toto.io/Statement/v1"
	// PredicateType lives under a path the project controls (plan §7.1, P0-6).
	PredicateType = "https://github.com/billiondollarsolo/cinema/task-result/v1"
	// MaxEnvelope bounds parsing before any allocation-heavy work (plan §7.2).
	MaxEnvelope = 64 << 10
)

// Statement is the signed payload.
type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Predicate `json:"predicate"`
}

type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type Predicate struct {
	SchemaVersion int       `json:"schema_version"`
	RunID         string    `json:"run_id"`
	RepositoryID  string    `json:"repository_id"`
	Task          TaskRef   `json:"task"`
	Source        SourceRef `json:"source"`
	EnvDigest     string    `json:"environment_digest"`
	VCS           string    `json:"vcs"` // commit for vcs_context: exact, else ""
	Platform      string    `json:"platform"`
	Result        Result    `json:"result"`
	Timing        Timing    `json:"timing"`
	Executor      Executor  `json:"executor"`
}

type TaskRef struct {
	ID             string `json:"id"`
	ContractDigest string `json:"contract_digest"`
}

type SourceRef struct {
	Digest         string `json:"digest"`
	ObservedCommit string `json:"observed_commit"` // informational; "" if dirty
	Capture        string `json:"capture"`         // workspace-observed | clean-checkout
}

type Result struct {
	Status   string `json:"status"` // only "passed" is ever signed
	ExitCode int    `json:"exit_code"`
	Origin   string `json:"origin"` // fresh | tool_cache
}

type Timing struct {
	StartedAt     time.Time `json:"started_at"`
	DurationMS    int64     `json:"duration_ms"`
	TimeAuthority string    `json:"time_authority"` // local-clock: self-asserted (plan §7.2)
}

type Executor struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	// Where it ran (plan §7.1). Container runs name the runner image, the arch
	// the code ran as and whether that was emulated. Never a hostname.
	Where    string `json:"where,omitempty"` // container | host-process
	Image    string `json:"image,omitempty"`
	Emulated bool   `json:"emulated,omitempty"`
}

// Envelope is a DSSE envelope.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// PAE is DSSE's pre-authentication encoding. Signatures cover exactly these
// bytes; the payload is never re-serialized before verifying (plan §7.2).
func PAE(payloadType string, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString("DSSEv1 ")
	b.WriteString(strconv.Itoa(len(payloadType)))
	b.WriteByte(' ')
	b.WriteString(payloadType)
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(len(payload)))
	b.WriteByte(' ')
	b.Write(payload)
	return b.Bytes()
}

// KeyID is the hex SHA-256 of the raw public key, truncated: a lookup hint only.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// Sign serializes st and signs it.
func Sign(st Statement, priv ed25519.PrivateKey) (Envelope, error) {
	if st.Predicate.Result.Status != "passed" {
		return Envelope{}, errors.New("evidence: only passed runs are signed")
	}
	payload, err := json.Marshal(st)
	if err != nil {
		return Envelope{}, err
	}
	if len(payload) > MaxEnvelope/2 {
		return Envelope{}, errors.New("evidence: statement too large")
	}
	sig := ed25519.Sign(priv, PAE(PayloadType, payload))
	return Envelope{PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: KeyID(priv.Public().(ed25519.PublicKey)), Sig: base64.StdEncoding.EncodeToString(sig)}}}, nil
}

// VerifiedStatement is a statement whose signature checked out against a
// trusted key. It says who claimed a result, not that the result is acceptable.
type VerifiedStatement struct {
	Statement
	Signer string // key ID of the trusted key that verified it
}

// Verification errors. Callers map them to reason codes.
var (
	ErrMalformed        = errors.New("evidence: malformed")
	ErrSignatureInvalid = errors.New("evidence: signature invalid")
	ErrSignerNotAllowed = errors.New("evidence: signer not allowed")
)

// Verify strictly parses raw envelope bytes and checks the signature against
// trusted keys. Any doubt is an error.
func Verify(raw []byte, trusted []ed25519.PublicKey) (VerifiedStatement, error) {
	var vs VerifiedStatement
	if len(raw) > MaxEnvelope {
		return vs, fmt.Errorf("%w: envelope over %d bytes", ErrMalformed, MaxEnvelope)
	}
	var env Envelope
	if err := strictUnmarshal(raw, &env); err != nil {
		return vs, fmt.Errorf("%w: envelope: %v", ErrMalformed, err)
	}
	if env.PayloadType != PayloadType || len(env.Signatures) == 0 || len(env.Signatures) > 4 {
		return vs, fmt.Errorf("%w: envelope fields", ErrMalformed)
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(env.Payload)
	if err != nil {
		return vs, fmt.Errorf("%w: payload encoding", ErrMalformed)
	}
	pae := PAE(env.PayloadType, payload)
	signer := ""
	for _, s := range env.Signatures {
		sig, err := base64.StdEncoding.Strict().DecodeString(s.Sig)
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		for _, k := range trusted { // keyid is only a hint: try every trusted key
			if ed25519.Verify(k, pae, sig) {
				signer = KeyID(k)
			}
		}
		if signer != "" {
			break
		}
	}
	if signer == "" {
		// A signature claiming a trusted key that doesn't verify is tampering;
		// anything else is a signer we don't trust. Both mean RUN.
		if anyKeyMatchesHint(env, trusted) {
			return vs, ErrSignatureInvalid
		}
		return vs, ErrSignerNotAllowed
	}
	// Only now parse the payload, from the exact signed bytes.
	if err := strictUnmarshal(payload, &vs.Statement); err != nil {
		return vs, fmt.Errorf("%w: statement: %v", ErrMalformed, err)
	}
	if err := vs.Statement.validate(); err != nil {
		return vs, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	vs.Signer = signer
	return vs, nil
}

// anyKeyMatchesHint reports whether any signature's keyid names a trusted key.
func anyKeyMatchesHint(env Envelope, trusted []ed25519.PublicKey) bool {
	for _, s := range env.Signatures {
		for _, k := range trusted {
			if s.KeyID == KeyID(k) {
				return true
			}
		}
	}
	return false
}

var (
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hexRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})?$`)
)

func (st *Statement) validate() error {
	p := st.Predicate
	switch {
	case st.Type != StatementType || st.PredicateType != PredicateType:
		return errors.New("statement type")
	case len(st.Subject) != 1 || len(st.Subject[0].Digest) != 1 || !hexRE.MatchString(st.Subject[0].Digest["sha256"]):
		return errors.New("subject")
	case st.Subject[0].Name != "cinema-action:"+p.Task.ID:
		return errors.New("subject name")
	case p.SchemaVersion != 1:
		return errors.New("schema_version")
	case p.RunID == "" || p.RepositoryID == "" || p.Task.ID == "" || p.Platform == "":
		return errors.New("missing field")
	case !digestRE.MatchString(p.Task.ContractDigest) || !digestRE.MatchString(p.Source.Digest) || !digestRE.MatchString(p.EnvDigest):
		return errors.New("digest format")
	case !commitRE.MatchString(p.VCS) || !commitRE.MatchString(p.Source.ObservedCommit):
		return errors.New("commit format")
	case p.Result.Status != "passed" || p.Result.ExitCode != 0:
		return errors.New("result is not a pass")
	case p.Result.Origin != "fresh" && p.Result.Origin != "tool_cache":
		return errors.New("result origin")
	case p.Source.Capture != "workspace-observed" && p.Source.Capture != "clean-checkout":
		return errors.New("source capture")
	case p.Timing.StartedAt.IsZero() || p.Timing.DurationMS < 0:
		return errors.New("timing")
	}
	return nil
}

// strictUnmarshal rejects unknown fields, duplicate keys and trailing data.
func strictUnmarshal(b []byte, v any) error {
	if err := rejectDuplicateKeys(b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// rejectDuplicateKeys walks the JSON token stream: encoding/json silently keeps
// the last duplicate, which lets two parsers see different statements.
func rejectDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var walk func(depth int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("nested too deeply")
		}
		t, err := dec.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := k.(string)
				if seen[key] {
					return fmt.Errorf("duplicate key %q", key)
				}
				seen[key] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for dec.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	return walk(0)
}
