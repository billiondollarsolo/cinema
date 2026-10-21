// P0-1 spike: the cheapest possible "everything reused" gate (plan §0.2, §16).
// Throwaway. Verifies one Ed25519 DSSE envelope so the workflow measures
// runner start + checkout + binary + verify, and nothing else.
//
//	gatestub keygen KEYFILE              → prints public key (base64)
//	gatestub sign KEYFILE PAYLOADFILE    → DSSE envelope JSON on stdout
//	gatestub verify PUBKEY_B64 ENVFILE   → exit 0 if valid, 1 if not
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

const payloadType = "application/vnd.in-toto+json"

type envelope struct {
	PayloadType string `json:"payloadType"`
	Payload     string `json:"payload"`
	Signatures  []struct {
		KeyID string `json:"keyid"`
		Sig   string `json:"sig"`
	} `json:"signatures"`
}

// pae is DSSE pre-authentication encoding: "DSSEv1 <len> <type> <len> <body>".
func pae(typ string, body []byte) []byte {
	b := []byte("DSSEv1 " + strconv.Itoa(len(typ)) + " " + typ + " " + strconv.Itoa(len(body)) + " ")
	return append(b, body...)
}

func main() {
	if len(os.Args) != 3 && len(os.Args) != 4 {
		usage()
	}
	switch os.Args[1] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		check(err)
		check(os.WriteFile(os.Args[2], priv.Seed(), 0o600))
		fmt.Println(base64.StdEncoding.EncodeToString(pub))
	case "sign":
		seed, err := os.ReadFile(os.Args[2])
		check(err)
		body, err := os.ReadFile(os.Args[3])
		check(err)
		sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), pae(payloadType, body))
		var e envelope
		e.PayloadType = payloadType
		e.Payload = base64.StdEncoding.EncodeToString(body)
		e.Signatures = append(e.Signatures, struct {
			KeyID string `json:"keyid"`
			Sig   string `json:"sig"`
		}{"", base64.StdEncoding.EncodeToString(sig)})
		check(json.NewEncoder(os.Stdout).Encode(e))
	case "verify":
		pub, err := base64.StdEncoding.DecodeString(os.Args[2])
		if err != nil || len(pub) != ed25519.PublicKeySize {
			fail("bad public key")
		}
		raw, err := os.ReadFile(os.Args[3])
		check(err)
		var e envelope
		if json.Unmarshal(raw, &e) != nil || e.PayloadType != payloadType || len(e.Signatures) == 0 {
			fail("MALFORMED") // ponytail: lax parse; strict parser (dup keys, unknown fields) is P2
		}
		body, err := base64.StdEncoding.DecodeString(e.Payload)
		if err != nil {
			fail("MALFORMED")
		}
		for _, s := range e.Signatures {
			sig, err := base64.StdEncoding.DecodeString(s.Sig)
			if err == nil && ed25519.Verify(pub, pae(e.PayloadType, body), sig) {
				fmt.Println("REUSE")
				return
			}
		}
		fail("SIGNATURE_INVALID")
	default:
		usage()
	}
}

func fail(reason string) { fmt.Println("RUN", reason); os.Exit(1) }

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(8)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gatestub keygen KEY | sign KEY PAYLOAD | verify PUB ENV")
	os.Exit(2)
}
