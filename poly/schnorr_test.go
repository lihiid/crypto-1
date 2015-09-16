package poly

import (
	"encoding/binary"
	"fmt"
	"testing"
)

var msg []byte = []byte("Hello World!")

func TestNewRound(t *testing.T) {
	n := 3
	pl := PolyInfo{edward, 2, n, n}
	schnorrs := generateSchnorrStructs(pl)
	randoms := generateSharedSecrets(pl)
	randoms2 := generateSharedSecrets(pl)
	for i, _ := range schnorrs {
		err := schnorrs[i].NewRound(randoms[i], msg)
		if err != nil {
			t.Error(fmt.Sprintf("NewRound should validate : %v", err))
		}
		err = schnorrs[i].NewRound(randoms2[i], msg)
		if err != nil {
			t.Error(fmt.Sprintf("Second NewRound should validate : %v", err))
		}
		if schnorrs[i].random.Pub.Equal(randoms[i].Pub) {
			t.Error(fmt.Sprintf("Random secret should not be equals"))
		}

	}
}

func TestRevealPartialSig(t *testing.T) {
	n := 3
	pl := PolyInfo{edward, 2, n, n}
	schnorrs := generateSchnorrStructs(pl)
	randoms := generateSharedSecrets(pl)
	for i, _ := range schnorrs {
		err := schnorrs[i].NewRound(randoms[i], msg)
		if err != nil {
			t.Error(fmt.Sprintf("NewRound should validate : %v", err))
		}
		ps := schnorrs[i].RevealPartialSig()
		if ps.Index != schnorrs[i].index() {
			t.Error(fmt.Sprintf("PartialSig does not have the same index as its producer ><"))
		}
		ps2 := schnorrs[i].RevealPartialSig()
		if !(*ps.Part).Equal(*ps2.Part) {
			t.Error(fmt.Sprintf("PartialSig does not return the same partial signature "))
		}
		err = schnorrs[i].verifyPartialSig(ps)
		if err != nil {
			t.Error(fmt.Sprintf("RevealPartialSig's sig could have not been verified : %v", err))
		}
	}
}

func TestAddPartialSig(t *testing.T) {
	n := 3
	pl := PolyInfo{edward, 2, n, n}
	schnorrs := generateSchnorrStructs(pl)
	randoms := generateSharedSecrets(pl)
	for i, _ := range schnorrs {
		err := schnorrs[i].NewRound(randoms[i], msg)
		if err != nil {
			t.Error(fmt.Sprintf("NewRound should validate : %v", err))
		}
	}
	for i, _ := range schnorrs {
		ps := schnorrs[i].RevealPartialSig()
		if ps.Index != schnorrs[i].index() {
			t.Error(fmt.Sprintf("PartialSig does not have the same index as its producer ><"))
		}
		// geive the partial sig to everyone
		for j, _ := range schnorrs {
			if err := schnorrs[j].AddPartialSig(ps); err != nil {
				t.Error(fmt.Sprintf("AddPartialSig should validate (adding partial sig of peer %d to peer %d : %v", ps.Index, schnorrs[j].index(), err))
			}
		}

	}

	// test adding again sig
	ps2 := schnorrs[0].RevealPartialSig()
	err := schnorrs[0].AddPartialSig(ps2)
	if err == nil {
		t.Error(fmt.Sprintf("AddPartialSig 2 times should return an error"))
	}
	// nillify an partial sig then add a wrong one to same index
	schnorrs[0].partials[ps2.Index] = nil
	s := edward.Secret().One()
	ps2.Part = &s
	err = schnorrs[0].AddPartialSig(ps2)
	if err == nil {
		t.Error(fmt.Sprintf("AddPartialSig with wrong sig should return an error"))
	}
}

func TestSchnorrSig(t *testing.T) {
	n := 3
	pl := PolyInfo{edward, 2, n, n}
	schnorrs := generateSchnorrStructs(pl)
	randoms := generateSharedSecrets(pl)
	for i, _ := range schnorrs {
		err := schnorrs[i].NewRound(randoms[i], msg)
		if err != nil {
			t.Error(fmt.Sprintf("NewRound should validate : %v", err))
		}
	}
	for i, _ := range schnorrs {
		ps := schnorrs[i].RevealPartialSig()
		if ps.Index != schnorrs[i].index() {
			t.Error(fmt.Sprintf("PartialSig does not have the same index as its producer ><"))
		}
		// geive the partial sig to everyone
		for j, _ := range schnorrs {
			if err := schnorrs[j].AddPartialSig(ps); err != nil {
				t.Error(fmt.Sprintf("AddPartialSig should validate (adding partial sig of peer %d to peer %d : %v", ps.Index, schnorrs[j].index(), err))
			}
		}
	}
	sig := make([]*SchnorrSig, n)
	for i, _ := range schnorrs {
		s, err := schnorrs[i].SchnorrSig()
		if err != nil {
			t.Error(fmt.Sprintf("SchnorrSig should validate : %v", err))
		}
		sig[i] = s
	}
	// test equality of the signature amongst the peers
	for i, _ := range sig {
		for j, _ := range sig[i+1:] {
			if !(*sig[i].signature).Equal(*sig[j].signature) {
				t.Error(fmt.Sprintf("SchnorrSig should produce the same signature amongst peer (%d vs %d)", i, j))
			}
			if !(sig[i].random.Equal(sig[j].random)) {
				t.Error(fmt.Sprintf("SchnorrSig should produce the same signature (random poly %d != %d", i, j))
			}
		}
	}
}

func TestVerifySchnorrSig(t *testing.T) {
	n := 3
	pl := PolyInfo{edward, 2, n, n}
	schnorrs := generateSchnorrStructs(pl)
	randoms := generateSharedSecrets(pl)
	for i, _ := range schnorrs {
		err := schnorrs[i].NewRound(randoms[i], msg)
		if err != nil {
			t.Error(fmt.Sprintf("NewRound should validate : %v", err))
		}
	}
	for i, _ := range schnorrs {
		ps := schnorrs[i].RevealPartialSig()
		if ps.Index != schnorrs[i].index() {
			t.Error(fmt.Sprintf("PartialSig does not have the same index as its producer ><"))
		}
		// geive the partial sig to everyone
		for j, _ := range schnorrs {
			if err := schnorrs[j].AddPartialSig(ps); err != nil {
				t.Error(fmt.Sprintf("AddPartialSig should validate (adding partial sig of peer %d to peer %d : %v", ps.Index, schnorrs[j].index(), err))
			}
		}
	}
	sig := make([]*SchnorrSig, n)
	for i, _ := range schnorrs {
		s, err := schnorrs[i].SchnorrSig()
		if err != nil {
			t.Error(fmt.Sprintf("SchnorrSig should validate : %v", err))
		}
		sig[i] = s
	}

	// Verify the signature amongst each peers
	for i, _ := range schnorrs {
		err := schnorrs[i].VerifySchnorrSig(sig[0], msg)
		if err != nil {
			t.Error(fmt.Sprintf("VerifySchnorrSig on peer %d should validate the signature : %v", i, err))
		}
	}
}

func TestPartialSchnorrSigMarshalling(t *testing.T) {
	n := 3
	pl := PolyInfo{edward, 2, n, n}
	schnorrs := generateSchnorrStructs(pl)
	randoms := generateSharedSecrets(pl)
	for i, _ := range schnorrs {
		err := schnorrs[i].NewRound(randoms[i], msg)
		if err != nil {
			t.Error(fmt.Sprintf("NewRound should validate : %v", err))
		}
	}
	ps := schnorrs[0].RevealPartialSig()
	buf, err := ps.MarshalBinary()
	if err != nil {
		t.Error(fmt.Sprintf("MarshalBinary on PartialSchnorrSig did not work : %v", err))
	}

	fmt.Printf("Buffer PartialSig length : %d, int64 len = %d,SecretLen = %d", len(buf), binary.Size(int64(3)), schnorrs[0].info.Suite.SecretLen())
	ps2 := schnorrs[0].EmptyPartialSig()
	err = ps2.UnmarshalBinary(buf)
	if err != nil {
		t.Error(fmt.Sprintf("UnmarshalBinary on PartialSchnorrSig did not work : %v", err))
	}
	if !ps.Equal(ps2) {
		t.Error(fmt.Sprintf("Unmarshalled partial sig should be equal to the original"))
	}

}

func TestSchnorrSigMarshalling(t *testing.T) {
	n := 3
	pl := PolyInfo{edward, 2, n, n}
	schnorrs := generateSchnorrStructs(pl)
	randoms := generateSharedSecrets(pl)
	for i, _ := range schnorrs {
		err := schnorrs[i].NewRound(randoms[i], msg)
		if err != nil {
			t.Error(fmt.Sprintf("NewRound should validate : %v", err))
		}
	}
	for i, _ := range schnorrs {
		ps := schnorrs[i].RevealPartialSig()
		if ps.Index != schnorrs[i].index() {
			t.Error(fmt.Sprintf("PartialSig does not have the same index as its producer ><"))
		}
		// geive the partial sig to everyone
		for j, _ := range schnorrs {
			if err := schnorrs[j].AddPartialSig(ps); err != nil {
				t.Error(fmt.Sprintf("AddPartialSig should validate (adding partial sig of peer %d to peer %d : %v", ps.Index, schnorrs[j].index(), err))
			}
		}
	}
	s, err := schnorrs[0].SchnorrSig()
	if err != nil {
		t.Error(fmt.Sprintf("SchnorrSig should validate : %v", err))
	}

	sbuf, err := s.MarshalBinary()
	if err != nil {
		t.Error(fmt.Sprintf("SchnorrSig had error while Marshalling %v", err))
	}
	s2 := schnorrs[0].EmptySchnorrSig()
	err = s2.UnmarshalBinary(sbuf)
	if err != nil {
		t.Error(fmt.Sprintf("SchnorrSig Unmarshaling should have been correct : %v", err))
	}

	if !s2.Equal(s) {
		t.Error(fmt.Sprintf("SchnorrSig structs should have been equals"))
	}

}