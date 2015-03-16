/* This package implements the Promise cryptographic primitive, which is based
 * on poly/sharing.go
 *
 * Failures are frequent in large-scale systems. When reliability is paramount,
 * the system requires some guarentee that it will still be able to make
 * progress in the midst of failures. To do so, recovering critical information
 * from failed nodes is often needed. Herein is the importance of this package.
 * The promise package provides such reliability in the area of private keys.
 *
 * If a server wishes to have extra reliability for a private key it is using,
 * it can construct a Promise struct. A Promise will take the private key and
 * shard it into secret shares via poly/sharing.go logic. The server can
 * then give the shares to a group of other servers who can act as insurers
 * of the Promise. The insurers will keep the secret shares. If the original
 * server ever goes offline, another server could ask the insurers for their
 * secret shares and then combine them into the original secret key. Hence, this
 * server could continue in the place of the original and the sytem can continue
 * to make progress.
 *
 * This file provides structs for handling the cryptographic logic of this
 * process. Other files can use these primitives to build a more robust
 * system. In particular, there are 4 structs:
 *
 *   1) Promise = respondible for sharding the secret, creating shares, and
 *                tracking which shares belong to which insurers
*
 *   2) PromiseState = responsible for keeping state about a given Promise such
 *                     as shares recovered and messages that either certify the
 *                     promise or prove that it is malicious
 *
 *   3) PromiseSignature = proves that an insurer has signed off on a Promise.
 *                         The signature could either be used to express
 *                         approval or disapproval
 *
 *   4) BlameProof = provides proof that a given Promise share was malicously
 *                   constructed. A valid BlameProof proves that the Promise
 *                   is untrustworthy and that the creator of the Promise is
 *                   malicious
 *
 * Further documentation for each of the different structs can be found below.
 * It is suggested to start with the Promise struct referring to the others
 * as necessary. Once a general knowledge of Promise is gained, the others
 * will make more sense.
 *
 * Code using this package will typically have the following flow (please see
 * "Key Terms" below for a definition of terms used):
 *
 * Step I: Take out the Promise
 *
 *   1) The promiser constructs a new Promise and stores it within a PromiseState.
 *
 * Step II: Certify the Promise
 *
 *   1) The promiser sends the Promise to the insurers.
 *
 *   2) The insurers verify the Promise is well-formed and make sure that their
 *      secret shares are valid.
 *
 *     a) If a secret share is invalid, an insurer creates a BlameProof and sends
 *        it back.
 *
 *     b) If the share is valid, an insurer creates a PromiseSignature to send
 *        to the promiser.
 *
 *   3) The promiser receives the message from the insurer.
 *
 *     a) If it is a valid BlameProof, the promiser must start all over and
 *        construct a non-malicious Promise (or, the system can ban this malicious
 *        promiser).
 *
 *     b) If the message is a PromiseSignature, the promiser can add the signature
 *        to its PromiseState.
 *
 *   4) Repeat steps 1-3 until the promiser has collected enough
 *      PromiseSignatures for the Promise to be certified.
 *
 * Step III: Distribute the Promise
 *
 *   1) Once the Promise is certified, the promiser can then send the promise to
 *      clients.
 *
 *   2) Clients can then request the signatures from the insurers to make sure
 *      the Promise is indeed certified.
 *
 *     a) This prevents a malicious promiser from simply leaving out valid
 *        BlameProofs and only sending good signatures to the clients.
 *
 *   3) Once the client receives enough signatures, the client will then trust
 *      the promiser to do work with the promised private key.
 *
 * Step IV: Perform work for Clients
 *
 * Step V: Reconstruct the Promised Secret (if the promiser goes down)
 *
 *   1) If the promiser is unresponsive for too long, a client can inform the
 *      insurers of the Promise.
 *
 *   2) The insurers can then check if the promiser is indeed unresponsive.
 *
 *     a) If so, the insurer reveal its share and sends it to the client.
 *
 *   3) The client repeats steps 1-2 until enough shares are recovered to
 *      reconstruct the secret.
 *
 *   4) The client reconstructs the secret and takes over for the promiser.
 *
 *
 *
 * Key Terms:
 *   promiser = the server making a Promise
 *   client   = recipients of a Promise who are trusting the promiser
 *   insurer  = servers who store secret shares of a promise. Such servers help
 *              during secret reconstruction.
 *
 *   Users of this code = programmers wishing to use this code in programs
*/
package promise

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strconv"

	"github.com/dedis/crypto/abstract"
	"github.com/dedis/crypto/anon"
	"github.com/dedis/crypto/config"
	"github.com/dedis/crypto/poly"
	"github.com/dedis/crypto/proof"
	"github.com/dedis/crypto/random"
)

// Used mostly in marshalling code, this is the size of a uint32
var uint32Size int = binary.Size(uint32(0))

// This is the protocol name used by crypto/proof verifiers and provers.
var protocolName string = "Promise Protocol"

// These are messages used for signatures
var sigMsg []byte = []byte("Promise Signature")
var sigBlameMsg []byte = []byte("Promise Blame Signature")

/* Promise structs are mechanisms by which a server can promise other servers
 * that an abstract.Secret will be availble even if the secret's owner goes
 * down. The secret to be promised will be sharded into shared secrets that can
 * be combined using Lagrange Interpolation to produce the original secret.
 * Other servers will act as insurers maintaining a share. If a client ever
 * needs the secret to be reconstructed, it can contact the insurers to regain
 * the shares and reconstruct the secret.
 *
 * The insurers and secrets arrays should remain synchronized. In other words,
 * insurers[i] and secrets[i] should both refer to the same server.
 *
 * Note to users of this code:
 *
 *   Here is a list of methods that should be called by each type of server:
 *
 * - Promisers
 *   * ConstructPromise
 *
 * - Insurers
 *   * VerifyShare
 *   * Sign
 *   * RevealShare
 *   * Blame
 *
 * - Clients
 *   * VerifyRevealedShare
 *
 * - All
 *   * UnmarshalInit
 *   * VerifySignature
 *   * VerifyBlame
 */
type Promise struct {

	// The id of the promise used to differentiate it from others
	// The id is the short term public key of the private key being promised
	id string

	// The cryptographic key suite used throughout the Promise.
	suite abstract.Suite

	// The minimum number of shares needed to reconstruct the secret
	t int

	// The minimum number of PromiseSignatures approving the Promise that
	// are needed before the Promise is certified. t <= r <= n
	r int

	// The total number of shares
	n int

	// The long-term public key of the promiser
	pubKey abstract.Point

	// The public polynomial that is used to verify the shared secrets
	pubPoly poly.PubPoly

	// A list of servers who will act as insurers of the Promise. The list
	// contains the long-term public keys of the insurers
	insurers []abstract.Point

	// The list of shared secrets to be sent to the insurers. They are
	// encrypted with Diffie-Hellman shared secrets between the insurer
	// and the promiser.
	secrets []abstract.Secret
}

/* Constructs a new Promise to guarentee a secret.
 *
 * Arguments
 *    secretPair   = the keypair of the secret to be promised
 *    longPair     = the long term keypair of the promiser
 *    t            = minimum number of shares needed to reconstruct the secret.
 *    r            = minimum PromiseSignatures needed to certify the Promise
 *    insurers     = a list of the long-term public keys of the insurers.
 *
 *
 * It is expected that:
 *
 *    t <= r <= len(insurers)
 *
 *    secretPair.Suite == longPair.Suite
 *
 * Returns
 *   A newly constructed Promise
 */
func (p *Promise) ConstructPromise(secretPair *config.KeyPair,
	longPair *config.KeyPair, t, r int, insurers []abstract.Point) *Promise {
	p.id       = secretPair.Public.String()
	p.t        = t
	p.r        = r
	p.n        = len(insurers)
	p.suite    = secretPair.Suite
	p.pubKey   = longPair.Public
	p.insurers = insurers
	p.secrets  = make([]abstract.Secret, p.n, p.n)
  
	// Verify that t <= r <= n
	if !(p.t <= r && p.r <= p.n){
		panic("Invalid t, r, and n. Expected t <= r <= n")
	}
	
	if longPair.Suite != secretPair.Suite {
		panic("Two different suites used.")
	}

	// Create the public polynomial and private shares. The number of shares
	// should be equal to the number of insurers.
	pripoly := new(poly.PriPoly).Pick(p.suite, p.t,
		secretPair.Secret, random.Stream)
	prishares := new(poly.PriShares).Split(pripoly, p.n)
	p.pubPoly = poly.PubPoly{}
	p.pubPoly.Commit(pripoly, nil)

	// Populate the secrets array with the shares encrypted by a Diffie-
	// Hellman shared secret between the promiser and appropriate insurer
	for i := 0; i < p.n; i++ {
		diffieBase := p.suite.Point().Mul(insurers[i], longPair.Secret)
		diffieSecret := p.diffieHellmanSecret(diffieBase)
		p.secrets[i] = p.suite.Secret().Add(prishares.Share(i),
				diffieSecret)
	}
	return p
}

/* Initializes a Promise for unmarshalling
 *
 * Arguments
 *    suite = the suite used within the Promise
 *
 * Returns
 *   An initialized Promise ready to be unmarshalled
 */
func (p *Promise) UnmarshalInit(suite abstract.Suite) *Promise {
	p.suite = suite
	return p
}

/* An internal helper used during unmarshalling, verifies that the Promise was
 * constructed correctly.
 *
 * Return
 *   an error if the promise is malformed, nil otherwise.
 *
 * TODO Consider more ways to verify (such as making sure there are no duplicate
 *      keys in p.insurers or that the promiser's long term public key is not in
 *      p.insurers).
 */
func (p *Promise) verifyPromise() error {
	// Verify t <= r <= n
	if p.t > p.n || p.t > p.r || p.r > p.n {
		return errors.New("Invalid t-of-n shares Promise. Expected: t <= r <= n")
	}
	// There should be a secret and public key for each of the n insurers.
	if len(p.insurers) != p.n || len(p.secrets) != p.n {
		return errors.New("Insurers and secrets array should be of length promise.n")
	}
	return nil
}

// Returns the id of the Promise
func (p *Promise) Id() string {
	return p.id
}

/* Given a Diffie-Hellman shared public key, produces a secret to encrypt
 * another secret
 *
 * Arguments
 *    diffieBase  = the DH shared public key
 *
 * Return
 *   the DH secret
 */
func (p *Promise) diffieHellmanSecret(diffieBase abstract.Point) abstract.Secret {
	buff, err := diffieBase.MarshalBinary()
	if err != nil {
		panic("Bad shared secret for Diffie-Hellman given.")
	}
	cipher := p.suite.Cipher(buff)
	return p.suite.Secret().Pick(cipher)
}


/* Verifies that a share has been properly constructed. This should be called by
 * insurers to verify that the share they received is valid constructed.
 *
 * Arguments
 *    i         = the index of the share to verify
 *    gKeyPair  = the long term key pair of the insurer of share i
 *
 * Return
 *  an error if the share is malformed, nil otherwise.
 *
 * Note
 *   Make sure that the proper index and key is specified.
 */
func (p *Promise) VerifyShare(i int, gKeyPair *config.KeyPair) error {
	if i < 0 || i >= p.n {
		return errors.New("Invalid index. Expected 0 <= i < n")
	}
	msg := "The long-term public key the Promise recorded as the insurer" +
		"of this shares differs from what is expected"
	if !p.insurers[i].Equal(gKeyPair.Public) {
		return errors.New(msg)
	}
	diffieBase := p.suite.Point().Mul(p.pubKey, gKeyPair.Secret)
	diffieSecret := p.diffieHellmanSecret(diffieBase)
	share := p.suite.Secret().Sub(p.secrets[i], diffieSecret)
	if !p.pubPoly.Check(i, share) {
		return errors.New("The share failed the public polynomial check.")
	}
	return nil
}

/* An internal helper function responsible for producing signatures
 *
 * Arguments
 *    i         = the index of the insurer's share
 *    gKeyPair  = the long term public/private keypair of the insurer.
 *    msg       = the message to sign
 *
 * Return
 *   A PromiseSignature object with the signature.
 */
func (p *Promise) sign(i int, gKeyPair *config.KeyPair, msg []byte) *PromiseSignature {
	set := anon.Set{gKeyPair.Public}
	sig := anon.Sign(gKeyPair.Suite, random.Stream, msg, set, nil, 0,
		gKeyPair.Secret)
	return new(PromiseSignature).init(gKeyPair.Suite, sig)
}

/* An internal helper function, verifies a signature is from a given insurer.
 *
 * Arguments
 *    i   = the index of the insurer in the insurers list
 *    sig = the PromiseSignature object containing the signature
 *    msg = the message that was signed
 *
 * Return
 *   an error if the signature is malformed, nil otherwise.
 */
func (p *Promise) verifySignature(i int, sig *PromiseSignature, msg []byte) error {
	if i < 0 || i >= p.n {
		return errors.New("Invalid index. Expected 0 <= i < n")
	}
	if sig.signature == nil {
		return errors.New("Nil PromiseSignature")
	}
	set := anon.Set{p.insurers[i]}
	_, err := anon.Verify(sig.suite, msg, set, nil, sig.signature)
	return err
}

/* A public wrapper function for sign, produces a signature for a given insurer.
 * Insurers should use this function to produce a PromiseSignature to express
 * approval of a Promise.
 *
 * Arguments
 *    i         = the index of the insurer's share
 *    gKeyPair  = the long term public/private keypair of the insurer.
 *
 * Return
 *   A PromiseSignature object with the signature.
 *
 * Note
 *   It is wise to first call VerifyShare to verify the insurer's share before
 *   signing on the promise.
 */
func (p *Promise) Sign(i int, gKeyPair *config.KeyPair) *PromiseSignature {
	return p.sign(i, gKeyPair, sigMsg)
}

/* Verifies a signature from a given insurer
 *
 * Arguments
 *    i   = the index of the insurer in the insurers list
 *    sig = the PromiseSignature object containing the signature
 *
 * Return
 *   an error if the promise is malformed, nil otherwise.
 */
func (p *Promise) VerifySignature(i int, sig *PromiseSignature) error {
	return p.verifySignature(i, sig, sigMsg)
}

/* Reveals the secret share that the insurer has been protecting. The insurer
 * should call this function on behalf of a client after verifying that the
 * Promiser is non-responsive.
 *
 * Arguments
 *    i        = the index of the insurer
 *    gkeyPair = the long-term keypair of the insurer
 *
 * Return
 *   the revealed private share
 */
func (p *Promise) RevealShare(i int, gKeyPair *config.KeyPair) abstract.Secret {
	diffieBase   := p.suite.Point().Mul(p.pubKey, gKeyPair.Secret)
	diffieSecret := p.diffieHellmanSecret(diffieBase)
	share        := p.suite.Secret().Sub(p.secrets[i], diffieSecret)
	return share
}

/* Verify that a revealed share is properly formed. This should be called by
 * clients or others who request an insurer to reveal its shared secret.
 *
 * Arguments
 *    i     = the index of the share
 *    share = the share to validate.
 *
 * Return
 *   Whether the secret is valid
 */
func (p *Promise) VerifyRevealedShare(i int, share abstract.Secret) error {
	if i < 0 || i >= p.n {
		return errors.New("Invalid index. Expected 0 <= i < n")
	}
	if !p.pubPoly.Check(i, share) {
		return errors.New("The share failed the public polynomial check.")
	}
	return nil
}

/* Create a proof that the promiser maliciously constructed a shared secret. An
 * insurer should call this if VerifyShare fails due to the public polynomial
 * check failing. If it failed for other reasons (such as a bad index) it is not
 * advised to call this function.
 *
 * Arguments
 *    i         = the index of the malicious shared secret
 *    gKeyPair  = the long term key pair of the insurer of share i
 *
 * Return
 *   A BlameProof that the promiser is malicious or nil if an error occurs
 *   An error object denoting the status of the proof construction
 */
func (p *Promise) Blame(i int, gKeyPair *config.KeyPair) (*BlameProof, error) {
	diffieKey := p.suite.Point().Mul(p.pubKey, gKeyPair.Secret)
	insurerSig := p.sign(i, gKeyPair, sigBlameMsg)

	choice := make(map[proof.Predicate]int)
	pred := proof.Rep("D", "x", "P")
	choice[pred] = 1
	rand := p.suite.Cipher(abstract.RandomKey)
	sval := map[string]abstract.Secret{"x": gKeyPair.Secret}
	pval := map[string]abstract.Point{"D": diffieKey, "P": p.pubKey}
	prover := pred.Prover(p.suite, sval, pval, choice)
	proof, err := proof.HashProve(p.suite, protocolName, rand, prover)
	if err != nil {
		return nil, err
	}
	return new(BlameProof).init(p.suite, diffieKey, proof, insurerSig), nil
}

/* Verifies that a BlameProof proves a share to be maliciously constructed.
 *
 * Arguments
 *    i     = the index of the share subject to blame
 *    proof = proof that alleges the promiser to have constructed a bad share.
 *
 * Return
 *   an error if the blame is unjustified or nil if the blame is justified.
 */
func (p *Promise) VerifyBlame(i int, blameProof *BlameProof) error {
	// Basic sanity checks
	if i < 0 || i >= p.n {
		return errors.New("Invalid index. Expected 0 <= i < n")
	}
	if err := p.verifySignature(i, &blameProof.signature, sigBlameMsg); err != nil {
		return err
	}

	// Verify the Diffie-Hellman shared secret was constructed properly
	pval := map[string]abstract.Point{"D": blameProof.diffieKey, "P": p.pubKey}
	pred := proof.Rep("D", "x", "P")
	verifier := pred.Verifier(p.suite, pval)
	err := proof.HashVerify(p.suite, protocolName, verifier,
		blameProof.diffieKeyProof)
	if err != nil {
		return err
	}

	// Verify the share is bad.
	diffieSecret := p.diffieHellmanSecret(blameProof.diffieKey)
	share        := p.suite.Secret().Sub(p.secrets[i], diffieSecret)
	if p.pubPoly.Check(i, share) {
		return errors.New("Unjustified blame. The share checks out okay.")
	}
	return nil
}

/* Tests whether two Promise structs are equal
 *
 * Arguments
 *    p2 = a pointer to the struct to test for equality
 *
 * Returns
 *   true if equal, false otherwise
 */
func (p *Promise) Equal(p2 *Promise) bool {
	if p.n != p2.n {
		return false
	}
	if p.suite != p2.suite {
		return false
	}
	for i := 0; i < p.n; i++ {
		if !p.secrets[i].Equal(p2.secrets[i]) ||
			!p.insurers[i].Equal(p2.insurers[i]) {
			return false
		}
	}
	return p.t == p2.t && p.r == p2.r &&
		p.pubKey.Equal(p2.pubKey) && p.pubPoly.Equal(&p2.pubPoly)
}

/* Returns the number of bytes used by this struct when marshalled
 *
 * Returns
 *   The marshal size
 *
 * Note
 *   Since the length of insurers and secrets is not known until after
 *   unmarshalling, do not call before unmarshalling.
 */
func (p *Promise) MarshalSize() int {
	return 3*uint32Size + p.suite.PointLen() + p.pubPoly.MarshalSize() +
		p.n*p.suite.PointLen() + p.n*p.suite.SecretLen()
}

/* Marshals a Promise struct into a byte array
 *
 * Returns
 *   A buffer of the marshalled struct
 *   The error status of the marshalling (nil if no error)
 *
 * Note
 *   The buffer is formatted as follows:
 *
 *      ||n||t||r||pubKey||pubPoly||==insurers_array==||==secrets==||
 *
 *   Remember: n == len(insurers) == len(secrets)
 */
func (p *Promise) MarshalBinary() ([]byte, error) {
	buf := make([]byte, p.MarshalSize())

	pointLen  := p.suite.PointLen()
	polyLen   := p.pubPoly.MarshalSize()
	secretLen := p.suite.SecretLen()

	// Encode n, r, t
	binary.LittleEndian.PutUint32(buf, uint32(p.n))
	binary.LittleEndian.PutUint32(buf[uint32Size:], uint32(p.t))
	binary.LittleEndian.PutUint32(buf[2*uint32Size:], uint32(p.r))

	// Encode pubKey and pubPoly
	pointBuf, err := p.pubKey.MarshalBinary()
	if err != nil {
		return nil, err
	}
	copy(buf[3*uint32Size:], pointBuf)

	polyBuf, err := p.pubPoly.MarshalBinary()
	if err != nil {
		return nil, err
	}
	copy(buf[3*uint32Size+pointLen:], polyBuf)

	// Encode the insurers and secrets array (Based on poly/sharing.go code)
	bufPos := 3*uint32Size + pointLen + polyLen
	for i := range p.insurers {
		pb, err := p.insurers[i].MarshalBinary()
		if err != nil {
			return nil, err
		}
		copy(buf[bufPos+i*pointLen:], pb)
	}
	bufPos += p.n * pointLen

	for i := range p.secrets {
		pb, err := p.secrets[i].MarshalBinary()
		if err != nil {
			return nil, err
		}
		copy(buf[bufPos+i*secretLen:], pb)
	}
	return buf, nil
}

/* Unmarshals a Promise from a byte buffer
 *
 * Arguments
 *    buf = the buffer containing the Promise
 *
 * Returns
 *   The error status of the unmarshalling (nil if no error)
 */
func (p *Promise) UnmarshalBinary(buf []byte) error {
	pointLen  := p.suite.PointLen()
	secretLen := p.suite.SecretLen()

	// Decode n, r, t
	p.n = int(binary.LittleEndian.Uint32(buf))
	p.t = int(binary.LittleEndian.Uint32(buf[uint32Size:]))
	p.r = int(binary.LittleEndian.Uint32(buf[2*uint32Size:]))

	bufPos := 3 * uint32Size

	// Decode pubKey and pubPoly
	p.pubKey = p.suite.Point()
	if err := p.pubKey.UnmarshalBinary(buf[bufPos : bufPos+pointLen]); err != nil {
		return err
	}
	bufPos += pointLen

	p.pubPoly = poly.PubPoly{}
	p.pubPoly.Init(p.suite, p.t, nil)
	polyLen := p.pubPoly.MarshalSize()
	if err := p.pubPoly.UnmarshalBinary(buf[bufPos : bufPos+polyLen]); err != nil {
		return err
	}
	bufPos += polyLen

	// Decode the insurers and secrets array (Based on poly/sharing.go code)
	p.insurers = make([]abstract.Point, p.n, p.n)
	for i := 0; i < p.n; i++ {
		start := bufPos + i*pointLen
		end := start + pointLen
		p.insurers[i] = p.suite.Point()
		if err := p.insurers[i].UnmarshalBinary(buf[start:end]); err != nil {
			return err
		}
	}
	bufPos += p.n * pointLen
	p.secrets = make([]abstract.Secret, p.n, p.n)
	for i := 0; i < p.n; i++ {
		start := bufPos + i*secretLen
		end := start + secretLen
		p.secrets[i] = p.suite.Secret()
		if err := p.secrets[i].UnmarshalBinary(buf[start:end]); err != nil {
			return err
		}
	}
	// Make sure the Promise is valid.
	return p.verifyPromise()
}

/* Marshals a Promise struct using an io.Writer
 *
 * Arguments
 *    w = the writer to use for marshalling
 *
 * Returns
 *   The number of bytes written
 *   The error status of the write (nil if no errors)
 */
func (p *Promise) MarshalTo(w io.Writer) (int, error) {
	buf, err := p.MarshalBinary()
	if err != nil {
		return 0, err
	}
	return w.Write(buf)
}

/* Unmarshals a Promise struct using an io.Reader
 *
 * Arguments
 *    r = the reader to use for unmarshalling
 *
 * Returns
 *   The number of bytes read
 *   The error status of the read (nil if no errors)
 */
func (p *Promise) UnmarshalFrom(r io.Reader) (int, error) {
	// Retrieve p.n and p.t and then initialize p.PubPoly
	buf := make([]byte, 2*uint32Size)
	n, err := io.ReadFull(r, buf)
	if err != nil {
		return n, err
	}
	p.n = int(binary.LittleEndian.Uint32(buf))
	p.t = int(binary.LittleEndian.Uint32(buf[uint32Size:]))
	p.pubPoly = poly.PubPoly{}
	p.pubPoly.Init(p.suite, p.t, nil)

	// MarshalSize can now be used to construct the final buffer. Copy
	// the contents into the buffer and unmarshal.
	finalBuf := make([]byte, p.MarshalSize())
	copy(finalBuf, buf)
	m, err := io.ReadFull(r, finalBuf[n:])
	if err != nil {
		return n + m, err
	}
	return n + m, p.UnmarshalBinary(finalBuf)
}

/* Returns a string representation of the Promise for easy debugging
 *
 * Returns
 *   The Promise's string representation
 */
func (p *Promise) String() string {
	s := "{Promise:\n"
	s += "Suite => " + p.suite.String() + ",\n"
	s += "t => " + strconv.Itoa(p.t) + ",\n"
	s += "r => " + strconv.Itoa(p.r) + ",\n"
	s += "n => " + strconv.Itoa(p.n) + ",\n"
	s += "Public Key => " + p.pubKey.String() + ",\n"
	s += "Public Polynomial => " + p.pubPoly.String() + ",\n"
	insurers := ""
	secrets := ""
	for i := 0; i < p.n; i++ {
		insurers += p.insurers[i].String() + ",\n"
		secrets += p.secrets[i].String() + ",\n"
	}
	s += "Insurers =>\n[" + insurers + "],\n"
	s += "Secrets =>\n[" + secrets + "]\n"
	s += "}\n"
	return s
}

/* The PromiseState struct is responsible for maintaining state about Promise
 * structs. It consists of three main pieces:
 *
 *    1. The promise itself, which should be treated like an immutable object
 *    2. The shared secrets the server has recovered so far
 *    3. A list of signatures from insurers cerifying the promise
 *
 * Each server should have one PromiseState per Promise
 *
 * Note to users of this code:
 *
 *    To add a share to PriShares, do:
 *
 *       p.PriShares.SetShare(index, share)
 *
 *    To reconstruct the secret, do:
 *
 *       p.PriShares.Secret()
 *
 *    Be warned that Secret will panic unless there are enough shares to
 *    reconstruct the secret. (See poly/sharing.go for more info)
 *
 * TODO Consider if it is worth adding a String function
 */
type PromiseState struct {

	// The actual promise
	Promise Promise

	// Primarily used by clients, contains shares the client has currently
	// obtained from insurers. This is what will be used to reconstruct the
	// promised secret.
	PriShares poly.PriShares

	// A list of signatures verifying that an insurer has cerified the
	// secret share it is guarding.
	signatures []*PromiseSignature

	// A list of blame proofs in which an insurer proves its secret share
	// to be malformed and hence proves the promiser of being malicious
	blames []*BlameProof
}

/* Initializes a new PromiseState
 *
 * Arguments
 *    promise = the promise to keep track of
 *
 * Returns
 *   An initialized PromiseState
 */
func (ps *PromiseState) Init(promise Promise) *PromiseState {
	ps.Promise = promise

	// Initialize a new PriShares based on information from the promise.
	ps.PriShares = poly.PriShares{}
	ps.PriShares.Empty(promise.suite, promise.t, promise.n)

	// There will be at most n signatures and blame proofs, one per insurer
	ps.signatures = make([]*PromiseSignature, promise.n, promise.n)
	ps.blames = make([]*BlameProof, promise.n, promise.n)
	return ps
}

/* Adds a signature from an insurer to the PromiseState
 *
 * Arguments
 *    i   = the index in the signature array this signature belongs
 *    sig = the PromiseSignature to add
 *
 * Postcondition
 *   The signature has been added
 *
 * Note to users of this code
 *   Be sure to call ps.Promise.VerifySignature before calling this function
 */
func (ps *PromiseState) AddSignature(i int, sig *PromiseSignature) {
	ps.signatures[i] = sig
}

/* Adds a blame proof from an insurer to the PromiseState
 *
 * Arguments
 *    i      = the index in the signature array this BlameProof belongs
 *    bproof = the BlameProof to add
 *
 * Postcondition
 *   The BlameProof has been added
 *
 * Note to users of this code
 *   Be sure to call ps.Promise.VerifyBlame before calling this function
 */
func (ps *PromiseState) AddBlameProof(i int, bproof *BlameProof) {
	ps.blames[i] = bproof
}

/* Checks whether the Promise object has received enough signatures to be
 * considered certified.
 *
 * Return
 *   an error denoting whether or not the Promise is certified.
 *     nil       == certified
 *     error     == not_yet_certified
 *
 * Note to users of this code
 *   An error here is not necessarily a cause for alarm, particularly if the
 *   Promise just needs more signatures. However, it could be a red flag if
 *   the error was caused by a valid BlameProof. A single valid BlameProof will
 *   permanently make a Promise uncertified.
 *
 * Technical Notes: The function goes through the list of signatures and checks
 *                  whether the signature is properly signed. If at least r of
 *                  these are signed and r is greater than t (the minimum number
 *                  of shares needed to reconstruct the secret), the promise is
 *                  considered valid. If any valid BlameProofs are found, the
 *                  Promise is automatically labelled uncertified.
 */
func (ps *PromiseState) PromiseCertified() error {
	if err := ps.Promise.verifyPromise(); err != nil {
		return err
	}
	validSigs := 0
	for i := 0; i < ps.Promise.n; i++ {
		// Check whether the PromiseSignatures and BlameProofs are
		// non-nil. Otherwise, bad things will happen.
		if ps.signatures[i] != nil &&
			ps.Promise.VerifySignature(i, ps.signatures[i]) == nil {
			validSigs += 1
		}

		if ps.blames[i] != nil &&
			ps.Promise.VerifyBlame(i, ps.blames[i]) == nil {
			return errors.New("A valid blame proofs proves this Promise to be uncertified.")
		}
	}
	if validSigs < ps.Promise.r {
		return errors.New("Not enough signatures yet to be certified")
	}
	return nil
}

/* The PromiseSignature struct is used by insurers to express their approval
 * or disapproval of a given promise. After receiving a promise and verifying
 * that their shares are good, insurers can produce a signature to send back
 * to the promiser. Alternatively, the insurers can produce a BlameProof (see
 * below) and use the signature to certify that they authored the blame.
 *
 * In order for a Promise to be considered certified, a promiser will need to
 * collect a certain amount of signatures from its insurers (please see the
 * Promise struct below for more details).
 *
 * Besides unmarshalling, users of this code do not need to worry about creating
 * a signature directly. Promise structs know how to generate signatures via
 * Promise.Sign
 */
type PromiseSignature struct {

	// The suite used for signing
	suite abstract.Suite

	// The signature proving that the insurer either approves or disapproves
	// of a Promise struct
	signature []byte
}

/* An internal function, initializes a new PromiseSignature
 *
 * Arguments
 *    suite = the signing suite
 *    sig   = the signature of approval
 *
 * Returns
 *   An initialized PromiseSignature
 */
func (p *PromiseSignature) init(suite abstract.Suite, sig []byte) *PromiseSignature {
	p.suite = suite
	p.signature = sig
	return p
}

/* For users of this code, initializes a PromiseSignature for unmarshalling
 *
 * Arguments
 *    suite = the signing suite
 *
 * Returns
 *   An initialized PromiseSignature ready to unmarshal a buffer
 */
func (p *PromiseSignature) UnmarshalInit(suite abstract.Suite) *PromiseSignature {
	p.suite = suite
	return p
}

/* Tests whether two PromiseSignature structs are equal
 *
 * Arguments
 *    p2 = a pointer to the struct to test for equality
 *
 * Returns
 *   true if equal, false otherwise
 */
func (p *PromiseSignature) Equal(p2 *PromiseSignature) bool {
	return p.suite == p2.suite && reflect.DeepEqual(p, p2)
}

/* Returns the number of bytes used by this struct when marshalled
 *
 * Returns
 *   The marshal size
 *
 * Note
 *   The function is only useful for a PromiseSignature struct that has already
 *   been unmarshalled. Since signatures can be of variable length, the marshal
 *   size is not known before unmarshalling. Do not call before unmarshalling.
 */
func (p *PromiseSignature) MarshalSize() int {
	return uint32Size + len(p.signature)
}

/* Marshals a PromiseSignature struct into a byte array
 *
 * Returns
 *   A buffer of the marshalled struct
 *   The error status of the marshalling (nil if no error)
 *
 * Note
 *   The buffer is formatted as follows:
 *
 *      ||Signature_Length||==Signature_Array===||
 */
func (p *PromiseSignature) MarshalBinary() ([]byte, error) {
	buf := make([]byte, p.MarshalSize())
	binary.LittleEndian.PutUint32(buf, uint32(len(p.signature)))
	copy(buf[uint32Size:], p.signature)
	return buf, nil
}

/* Unmarshals a PromiseSignature from a byte buffer
 *
 * Arguments
 *    buf = the buffer containing the PromiseSignature
 *
 * Returns
 *   The error status of the unmarshalling (nil if no error)
 */
func (p *PromiseSignature) UnmarshalBinary(buf []byte) error {
	if len(buf) < uint32Size {
		return errors.New("Buffer size too small")
	}

	sigLen := int(binary.LittleEndian.Uint32(buf))
	if len(buf) < uint32Size+sigLen {
		return errors.New("Buffer size too small")
	}

	p.signature = buf[uint32Size : uint32Size+sigLen]
	return nil
}

/* Marshals a PromiseSignature struct using an io.Writer
 *
 * Arguments
 *    w = the writer to use for marshalling
 *
 * Returns
 *   The number of bytes written
 *   The error status of the write (nil if no errors)
 */
func (p *PromiseSignature) MarshalTo(w io.Writer) (int, error) {
	buf, err := p.MarshalBinary()
	if err != nil {
		return 0, err
	}
	return w.Write(buf)
}

/* Unmarshal a PromiseSignature struct using an io.Reader
 *
 * Arguments
 *    r = the reader to use for unmarshalling
 *
 * Returns
 *   The number of bytes read
 *   The error status of the read (nil if no errors)
 */
func (p *PromiseSignature) UnmarshalFrom(r io.Reader) (int, error) {
	// Retrieve the signature length from the reader
	buf := make([]byte, uint32Size)
	n, err := io.ReadFull(r, buf)
	if err != nil {
		return n, err
	}

	sigLen := int(binary.LittleEndian.Uint32(buf))

	// Calculate the length of the entire message and create the new buffer.
	finalBuf := make([]byte, uint32Size+sigLen)

	// Copy the old buffer into the new
	copy(finalBuf, buf)

	// Read the rest and unmarshal.
	m, err := io.ReadFull(r, finalBuf[n:])
	if err != nil {
		return n + m, err
	}
	return n + m, p.UnmarshalBinary(finalBuf)
}

/* Returns a string representation of the PromiseSignature for easy debugging
 *
 * Returns
 *   The PromiseSignature's string representation
 */
func (p *PromiseSignature) String() string {
	s := "{PromiseSignature:\n"
	s += "Suite => " + p.suite.String() + ",\n"
	s += "Signature => " + hex.EncodeToString(p.signature) + "\n"
	s += "}\n"
	return s
}

/* The BlameProof struct provides an accountability measure. If a promiser
 * decides to construct a faulty share, insurers can construct a BlameProof
 * to show that the promiser is malicious.
 *
 * The insurer provides the Diffie-Hellman shared secret with the promiser so
 * that others can decode the share in question. A zero knowledge proof is
 * provided to prove that the shared secret was constructed properly. Lastly, a
 * PromiseSignature is attached to prove that the insurer endorses the blame.
 * When other servers receive the BlameProof, they can then verify whether the
 * promiser is malicious or the insurer is falsely accusing the promiser.
 *
 * To quickly summarize the blame procedure, the following must hold for the
 * blame to succeed:
 *
 *   1. The PromiseSignature must be valid
 *
 *   2. The Diffie-Hellman key must be verified to be correct
 *
 *   3. The insurer's share when decrypted must fail the PubPoly.Check of
 *   the Promise struct
 *
 * If all hold, the promiser is proven malicious. Otherwise, the insurer is
 * slanderous.
 *
 * Beyond unmarshalling, users of this code need not worry about constructing a
 * BlameProof struct themselves. The Promise struct knows how to create a
 * BlameProof via the Promise.Blame method.
 */
type BlameProof struct {

	// The suite used throughout the BlameProof
	suite abstract.Suite

	// The Diffie-Hellman shared secret between the insurer and promiser
	diffieKey abstract.Point

	// A HashProve proof that the insurer properly constructed the Diffie-
	// Hellman shared secret
	diffieKeyProof []byte

	// The signature denoting that the insurer approves of the blame
	signature PromiseSignature
}

/* An internal function, initializes a new BlameProof struct
 *
 * Arguments
 *    suite = the suite used for the Diffie-Hellman key, proof, and signature
 *    key   = the shared Diffie-Hellman key
 *    dkp   = the proof validating the Diffie-Hellman key
 *    sig   = the insurer's signature
 *
 * Returns
 *   An initialized BlameProof
 */
func (bp *BlameProof) init(suite abstract.Suite, key abstract.Point,
	dkp []byte, sig *PromiseSignature) *BlameProof {
	bp.suite = suite
	bp.diffieKey = key
	bp.diffieKeyProof = dkp
	bp.signature = *sig
	return bp
}

/* Initializes a BlameProof struct for unmarshalling
 *
 * Arguments
 *    s = the suite used for the Diffie-Hellman key, proof, and signature
 *
 * Returns
 *   An initialized BlameProof ready to be unmarshalled
 */
func (bp *BlameProof) UnmarshalInit(suite abstract.Suite) *BlameProof {
	bp.suite = suite
	return bp
}

/* Tests whether two BlameProof structs are equal
 *
 * Arguments
 *    bp2 = a pointer to the struct to test for equality
 *
 * Returns
 *   true if equal, false otherwise
 */
func (bp *BlameProof) Equal(bp2 *BlameProof) bool {
	return bp.suite == bp2.suite &&
		bp.diffieKey.Equal(bp2.diffieKey) &&
		reflect.DeepEqual(bp.diffieKeyProof, bp2.diffieKeyProof) &&
		bp.signature.Equal(&bp2.signature)
}

/* Returns the number of bytes used by this struct when marshalled
 *
 * Returns
 *   The marshal size
 *
 * Note
 *   Since PromiseSignature structs and the Diffie-Hellman proof can be of
 *   variable length, this function is only useful for a BlameProof that is
 *   already unmarshalled. Do not call before unmarshalling.
 */
func (bp *BlameProof) MarshalSize() int {
	return 2*uint32Size + bp.suite.PointLen() + len(bp.diffieKeyProof) +
		bp.signature.MarshalSize()
}

/* Marshals a BlameProof struct into a byte array
 *
 * Returns
 *   A buffer of the marshalled struct
 *   The error status of the marshalling (nil if no error)
 *
 * Note
 *   The buffer is formatted as follows:
 *
 *   ||Diffie_Key_Proof_Length||PromiseSignature_Length||Diffie_Key||
 *      Diffie_Key_Proof||PromiseSignature||
 */
func (bp *BlameProof) MarshalBinary() ([]byte, error) {
	pointLen := bp.suite.PointLen()
	proofLen := len(bp.diffieKeyProof)
	buf := make([]byte, bp.MarshalSize())

	binary.LittleEndian.PutUint32(buf, uint32(proofLen))
	binary.LittleEndian.PutUint32(buf[uint32Size:],
		uint32(bp.signature.MarshalSize()))

	pointBuf, err := bp.diffieKey.MarshalBinary()
	if err != nil {
		return nil, err
	}
	copy(buf[2*uint32Size:], pointBuf)
	copy(buf[2*uint32Size+pointLen:], bp.diffieKeyProof)

	sigBuf, err := bp.signature.MarshalBinary()
	if err != nil {
		return nil, err
	}
	copy(buf[2*uint32Size+pointLen+proofLen:], sigBuf)
	return buf, nil
}

/* Unmarshals a BlameProof from a byte buffer
 *
 * Arguments
 *    buf = the buffer containing the BlameProof
 *
 * Returns
 *   The error status of the unmarshalling (nil if no error)
 */
func (bp *BlameProof) UnmarshalBinary(buf []byte) error {
	// Verify the buffer is large enough for the diffie proof length
	// (uint32), the PromiseSignature length (uint32), and the
	// Diffie-Hellman shared secret (abstract.Point)
	pointLen := bp.suite.PointLen()
	if len(buf) < 2*uint32Size+pointLen {
		return errors.New("Buffer size too small")
	}
	proofLen := int(binary.LittleEndian.Uint32(buf))
	sigLen := int(binary.LittleEndian.Uint32(buf[uint32Size:]))

	bufPos := 2 * uint32Size
	bp.diffieKey = bp.suite.Point()
	if err := bp.diffieKey.UnmarshalBinary(buf[bufPos : bufPos+pointLen]); err != nil {
		return err
	}
	bufPos += pointLen

	if len(buf) < 2*uint32Size+pointLen+proofLen+sigLen {
		return errors.New("Buffer size too small")
	}
	bp.diffieKeyProof = make([]byte, proofLen, proofLen)
	copy(bp.diffieKeyProof, buf[bufPos:bufPos+proofLen])
	bufPos += proofLen

	bp.signature = PromiseSignature{}
	bp.signature.UnmarshalInit(bp.suite)
	if err := bp.signature.UnmarshalBinary(buf[bufPos : bufPos+sigLen]); err != nil {
		return err
	}
	return nil
}

/* Marshals a BlameProof struct using an io.Writer
 *
 * Arguments
 *    w = the writer to use for marshalling
 *
 * Returns
 *   The number of bytes written
 *   The error status of the write (nil if no errors)
 */
func (bp *BlameProof) MarshalTo(w io.Writer) (int, error) {
	buf, err := bp.MarshalBinary()
	if err != nil {
		return 0, err
	}
	return w.Write(buf)
}

/* Unmarshals a BlameProof struct using an io.Reader
 *
 * Arguments
 *    r = the reader to use for unmarshalling
 *
 * Returns
 *   The number of bytes read
 *   The error status of the read (nil if no errors)
 */
func (bp *BlameProof) UnmarshalFrom(r io.Reader) (int, error) {
	// Retrieve the proof length and signature length from the reader
	buf := make([]byte, 2*uint32Size)
	n, err := io.ReadFull(r, buf)
	if err != nil {
		return n, err
	}
	pointLen := bp.suite.PointLen()
	proofLen := int(binary.LittleEndian.Uint32(buf))
	sigLen := int(binary.LittleEndian.Uint32(buf[uint32Size:]))

	// Calculate the final buffer, copy the old data to it, and fill it
	// for unmarshalling
	finalLen := 2*uint32Size + pointLen + proofLen + sigLen
	finalBuf := make([]byte, finalLen)
	copy(finalBuf, buf)
	m, err := io.ReadFull(r, finalBuf[n:])
	if err != nil {
		return n + m, err
	}
	return n + m, bp.UnmarshalBinary(finalBuf)
}

/* Returns a string representation of the BlameProof for easy debugging
 *
 * Returns
 *   The BlameProof's string representation
 */
func (bp *BlameProof) String() string {
	proofHex := hex.EncodeToString(bp.diffieKeyProof)
	s := "{BlameProof:\n"
	s += "Suite => " + bp.suite.String() + ",\n"
	s += "Diffie-Hellman Shared Secret => " + bp.diffieKey.String() + ",\n"
	s += "Diffie-Hellman Proof => " + proofHex + ",\n"
	s += "PromiseSignature => " + bp.signature.String() + "\n"
	s += "}\n"
	return s
}
