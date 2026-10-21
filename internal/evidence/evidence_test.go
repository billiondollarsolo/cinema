package evidence

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func key(t testing.TB) ed25519.PrivateKey {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

func statement() Statement {
	d := "sha256:" + strings.Repeat("ab", 32)
	return Statement{
		Type: StatementType, PredicateType: PredicateType,
		Subject: []Subject{{Name: "cinema-action:unit", Digest: map[string]string{"sha256": strings.Repeat("cd", 32)}}},
		Predicate: Predicate{
			SchemaVersion: 1, RunID: "run_1", RepositoryID: "github.com/123", Platform: "linux/amd64",
			Task:      TaskRef{ID: "unit", ContractDigest: d},
			Source:    SourceRef{Digest: d, Capture: "clean-checkout", ObservedCommit: strings.Repeat("e", 40)},
			EnvDigest: d,
			Result:    Result{Status: "passed", ExitCode: 0, Origin: "fresh"},
			Timing:    Timing{StartedAt: time.Unix(1_800_000_000, 0).UTC(), DurationMS: 42000, TimeAuthority: "local-clock"},
			Executor:  Executor{Kind: "cinema-cli", Version: "0.1.0"},
		},
	}
}

func signRaw(t testing.TB, st Statement, k ed25519.PrivateKey) []byte {
	env, err := Sign(st, k)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return b
}

func TestPAEVector(t *testing.T) { // DSSE spec example
	if got := string(PAE("http://example.com/HelloWorld", []byte("hello world"))); got != "DSSEv1 29 http://example.com/HelloWorld 11 hello world" {
		t.Fatalf("PAE = %q", got)
	}
}

func TestRoundTrip(t *testing.T) {
	k := key(t)
	vs, err := Verify(signRaw(t, statement(), k), []ed25519.PublicKey{pub(key(t)), pub(k)})
	if err != nil {
		t.Fatal(err)
	}
	if vs.Signer != KeyID(pub(k)) || vs.Predicate.Task.ID != "unit" {
		t.Fatalf("got %+v", vs)
	}
}

func TestTamper(t *testing.T) { // ACC-023
	k := key(t)
	var env Envelope
	json.Unmarshal(signRaw(t, statement(), k), &env)
	payload, _ := base64.StdEncoding.DecodeString(env.Payload)
	payload[len(payload)/2] ^= 1
	env.Payload = base64.StdEncoding.EncodeToString(payload)
	raw, _ := json.Marshal(env)
	if _, err := Verify(raw, []ed25519.PublicKey{pub(k)}); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("flipped payload: %v", err)
	}
}

func TestUntrustedSigner(t *testing.T) { // ACC-024, ACC-028
	k := key(t)
	if _, err := Verify(signRaw(t, statement(), k), []ed25519.PublicKey{pub(key(t))}); !errors.Is(err, ErrSignerNotAllowed) {
		t.Fatalf("untrusted: %v", err)
	}
	if _, err := Verify(signRaw(t, statement(), k), nil); !errors.Is(err, ErrSignerNotAllowed) {
		t.Fatalf("no trusted keys (key removed): %v", err)
	}
}

func TestOnlyPassesAreSigned(t *testing.T) {
	st := statement()
	st.Predicate.Result.Status = "failed"
	if _, err := Sign(st, key(t)); err == nil {
		t.Fatal("signed a failure")
	}
}

// signPayload signs arbitrary bytes, to build validly signed but malformed statements.
func signPayload(k ed25519.PrivateKey, payload []byte) []byte {
	env := Envelope{PayloadType: PayloadType, Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{{KeyID: KeyID(pub(k)), Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(k, PAE(PayloadType, payload)))}}}
	b, _ := json.Marshal(env)
	return b
}

func TestMalformed(t *testing.T) { // ACC-026, ACC-027
	k := key(t)
	good, _ := json.Marshal(statement())
	trusted := []ed25519.PublicKey{pub(k)}
	mutate := func(f func(*Statement)) []byte {
		st := statement()
		f(&st)
		b, _ := json.Marshal(st)
		return b
	}
	cases := map[string][]byte{
		"duplicate key":   []byte(strings.Replace(string(good), `"_type":`, `"_type":"x","_type":`, 1)),
		"unknown field":   []byte(strings.Replace(string(good), `{"_type"`, `{"extra":1,"_type"`, 1)),
		"trailing data":   append(append([]byte{}, good...), []byte(` {}`)...),
		"wrong predicate": mutate(func(s *Statement) { s.PredicateType = "https://evil/x" }),
		"bad digest":      mutate(func(s *Statement) { s.Predicate.Source.Digest = "sha256:xyz" }),
		"failed result":   mutate(func(s *Statement) { s.Predicate.Result = Result{Status: "failed", ExitCode: 1, Origin: "fresh"} }),
		"two subjects":    mutate(func(s *Statement) { s.Subject = append(s.Subject, s.Subject[0]) }),
		"subject name":    mutate(func(s *Statement) { s.Subject[0].Name = "cinema-action:other" }),
		"no run id":       mutate(func(s *Statement) { s.Predicate.RunID = "" }),
	}
	for name, payload := range cases {
		if _, err := Verify(signPayload(k, payload), trusted); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Envelope-level problems.
	raw := signRaw(t, statement(), k)
	for name, b := range map[string][]byte{
		"oversized":     []byte(`{"payload":"` + strings.Repeat("A", MaxEnvelope) + `"}`),
		"envelope dup":  []byte(strings.Replace(string(raw), `"payloadType":`, `"payloadType":"x","payloadType":`, 1)),
		"no signatures": []byte(`{"payloadType":"` + PayloadType + `","payload":"e30=","signatures":[]}`),
		"not json":      []byte(`nope`),
	} {
		if _, err := Verify(b, trusted); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func FuzzVerify(f *testing.F) {
	k := key(f)
	f.Add(signRaw(f, statement(), k))
	f.Add([]byte(`{"payloadType":"x","payload":"","signatures":[{"keyid":"","sig":""}]}`))
	trusted := []ed25519.PublicKey{pub(k)}
	f.Fuzz(func(t *testing.T, raw []byte) {
		vs, err := Verify(raw, trusted)
		if err == nil && (vs.Predicate.Result.Status != "passed" || vs.Signer == "") {
			t.Fatalf("accepted %+v", vs)
		}
	})
}
