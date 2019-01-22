// Package vrf implements an RSA-based Verifiable Random Function
//
//
// Usage
//
//
// Generate(key, seed) will generate a Proof object. Its Output field contains
// the VRF output for the given seed and key. The rest of the fields provide the
// proof that Output was generated as mandated by the key and seed. To those who
// don't know the secret key, Output values should be statistically
// indistinguishable from uniform random samples from {0, ..., 2**256-1}.
//
// Given a Proof object p, use p.Verify() to check that p is correct, or pass
// its fields to the corresponding arguments of VRF.sol's isValidVRFOutput
// method for on-chain verification.
//
// Private keys with the required PublicExponent and key size can be generated
// with code like
//
//   key, err := MakeKey()
//   if err != nil { panic(err) }
//
// The prime factors used in key are safe primes
// (https://en.wikipedia.org/wiki/Safe_prime), meaning (p-1)/2, (q-1)/2 are
// should also be prime. Searching for these is a bit slow, so allow MakeKey a
// couple of minutes.
//
//
// Changing the key size
//
//
// The key size and public exponent used for this protocol are set in the
// constants KeySizeBits and PublicExponent. Recompile with a different key
// size, if necessary. Note that this will require a corresponding change to the
// on-chain contract, VRF.sol, and its tests, VRF_test.js. We don't recommend
// changing PublicExponent: any change will at least double the gas cost for
// on-chain verification. Precautions have been taken (in seedToRingValue) to
// mitigate the risk of using a small public exponent.
//
//
// How it works
//
//
// An RSA key (modulus, secretExponent, publicExponent), operates over
// {0,...,modulus-1}, a ring known as ℤ/(modulus)ℤ where multiplication is
// mult(a,b)=((a*b)%modulus), and (%,*) are the usual integer operations. The
// mult operation can be used to define exponentiation, exp(a, 0) = 1,
// exp(a, n) = mult(a, exp(a, n-1)) for n > 0.
//
// Given a uint256 α, the owner of the seed uses Generate to publish
// π=exp(seedToRingValue(α),secretExponent). This is used to produce a
// pseudo-random value hash(π) which can't be constructed without knowing both
// the seed and the secret key. Anyone can check that the value was produced as
// mandated, though, by checking the hash and that
// π=exp(seedToRingValue(α),publicExponent).
//
// This implementation works almost as described in
// https://tools.ietf.org/html/draft-irtf-cfrg-vrf-03#section-4.1 and
// https://eprint.iacr.org/2017/099.pdf (figure 1, section 4, and Appendix C),
// but there are some modifications which make on-chain verification cheaper.
//
//
// Differences from the RSA VRF in https://eprint.iacr.org/2017/099.pdf
//
//
// Note that as long as the outputs are deterministic and statistically
// indistinguishable without knowledge of the seed, and collision-resistant, the
// security proofs given in Appendix C of https://eprint.iacr.org/2017/099.pdf
// will hold for this implementation..
//
// Where we use seedToRingValue(α), Figure 1 recommends using MGF1(α):
// https://tools.ietf.org/html/rfc2437#section-10.2.1 . Instead of appending a
// counter to a bytes representation of the MGF1 seed as recommended there, we
// recursively hash the last hash output, starting from seed (as a big-endian
// 256-bit word), to generate the 256-bit words representing the ring value.
// I.e., the 256-bit words initially generated by seedToRingValue are
// [hash(α),hash(hash(α)),etc.] (Then the top bit in the first word is turned
// off, per spec.) This is a little more efficient in solidity, and its output
// is statistically indistinguishable from the MGF1 output unless the seed is
// known.
//
// For the hash function, we use keccak256 (AKA SHA3), rather than SHA1 or
// MD{2,5}, as allowed in that spec, which is a bit out of date. The latest IETF
// RSA spec, https://tools.ietf.org/html/rfc8017#appendix-B.1 , allows MD{2,5}
// and SHA{1,224,256,384,512{,/{224,256}}}. As long as keccak256 outputs are
// collision-resistant and statistically indistinguishable from
// SHA{256,384/256,512/256} given ignorance of the inputs, this should have no
// impact on security.
package vrf

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/smartcontractkit/chainlink/utils"
	"go.uber.org/multierr"
)

// KeySizeBits is the number of bits expected in RSA VRF keys.
// Any change to this forces change to VRF.sol and VRF_test.js, too.
const KeySizeBits = 2048
const keySizeBytes = KeySizeBits / 8
const keySizeWords = KeySizeBits / 256

const wordBytes = 32

// PublicExponent is the exponent used in RSA public keys
// Any change to this forces change to VRF.sol and VRF_test.js, too.
//
// A public exponent of 3 is very cheap to calculate, in terms of ethereum gas,
// and no risk for this application because the RSA input produced by
// seedToRingValue is almost uniformly randomly sampled from the ring.
const PublicExponent = 3

// Proof represents a proof that Output was generated as mandated by the Key
// from the given Seed, via Decryption.
type Proof struct {
	Key                      *rsa.PublicKey
	Seed, Decryption, Output *big.Int
}

// panicUnless is a basic golang "assert", used for conditions which should
// never fail.
func panicUnless(prop bool, message error) {
	if !prop {
		panic(message)
	}
}

var zero, one, two, three = big.NewInt(0), big.NewInt(1), big.NewInt(2), big.NewInt(3)

// Textbook RSA "decryption", copied from crypto/rsa.go/decypt function. Returns
// seed raised to the private exponent of k, k's modulus. Uses faster CRT method
// if enabled on k.
func decrypt(k *rsa.PrivateKey, seed *big.Int) *big.Int {
	panicUnless(len(k.Primes) == 2,
		errors.New("the RSA VRF only works with two-factor moduli"))
	if k.Precomputed.Dp == nil { // Do it the slow way
		return new(big.Int).Exp(seed, k.D, k.N)
	}
	// We have the precalculated values needed for the CRT.
	m := new(big.Int).Exp(seed, k.Precomputed.Dp, k.Primes[0])
	m2 := new(big.Int).Exp(seed, k.Precomputed.Dq, k.Primes[1])
	m.Sub(m, m2)
	if m.Sign() < 0 {
		m.Add(m, k.Primes[0])
	}
	m.Mul(m, k.Precomputed.Qinv)
	m.Mod(m, k.Primes[0])
	m.Mul(m, k.Primes[1])
	m.Add(m, m2)
	return m
}

// encrypt "encrypts" m under the publicKey, with textbook RSA.
//
// This is inadequate for actual encryption. Use rsa.EncryptOAEP for that. The
// point here is not to encrypt sensitive data, but to use an operation which
// can only be inverted with knowledge of the private key.
func encrypt(publicKey *rsa.PublicKey, m *big.Int) *big.Int {
	exponentBig := new(big.Int).SetUint64(uint64(publicKey.E))
	return new(big.Int).Exp(m, exponentBig, publicKey.N)
}

// asUint256 returns i represented as an array of packed uint256's, a la solidity
func asUint256Array(i *big.Int) []byte {
	inputBytes := i.Bytes()
	outputBytesDeficit := wordBytes - (len(inputBytes) % wordBytes)
	if outputBytesDeficit == wordBytes {
		outputBytesDeficit = 0
	}
	rv := append(make([]byte, outputBytesDeficit), inputBytes...)
	panicUnless(
		len(rv)%wordBytes == 0 &&
			i.Cmp(new(big.Int).SetBytes(rv)) == 0,
		errors.New("rv is not i as big-endian uint256 array"))
	return rv
}

func asKeySizeUint256Array(i *big.Int) []byte {
	a := asUint256Array(i)
	rv := append(make([]byte, (keySizeBytes-len(a))), a...)
	panicUnless(len(rv) == keySizeBytes,
		errors.New("i must fit in keySizeBytes"))
	panicUnless(len(rv)%wordBytes == 0,
		errors.New("must generate packed uint256 array"))
	panicUnless(i.Cmp(new(big.Int).SetBytes(rv)) == 0,
		errors.New("must represent i"))
	return rv
}

// decryptionToOutput generates the actual randomness to be output by the VRF,
// from the output of the RSA "decryption"
func decryptionToOutput(decryption *big.Int) (*big.Int, error) {
	decrypt := asKeySizeUint256Array(decryption)
	output, err := utils.Keccak256(decrypt)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(output), nil
}

// seedToRingValue hashes seed into ℤ/(k.N)ℤ, represented as an integer in
// {0,..., k.N-1}.
//
// This plays the same role as the Mask Generation Function, MGF1, in
// https://tools.ietf.org/html/draft-irtf-cfrg-vrf-03#section-4.1 , and the same
// role as padding, in regular RSA.
//
// Forcing the VRF prover to "decrypt" this, rather than the initial seed
// itself, prevents an adversary from submitting arbitrary "proofs" that, for
// instance, back out a seed from encrypting a "decrypt" using the public key,
// or exploiting simple arithmetic relationships which hold over the naturals
// and hence all moduli, like 2³≡8 mod k.N. See also
// https://www.di.ens.fr/~fouque/ens-rennes/coppersmith.pdf
func seedToRingValue(seed *big.Int, k *rsa.PublicKey) (*big.Int, error) {
	seedBytes := asUint256Array(seed)
	if len(seedBytes) != wordBytes || seed.Cmp(big.NewInt(0)) == -1 {
		return nil, fmt.Errorf("Seed must fit in uint256")
	}
	if k.N.BitLen() > KeySizeBits || k.N.BitLen()%256 != 0 {
		return nil, fmt.Errorf("public modulus must fit in key size, " +
			"which must be a multiple of 256 bits")
	}
	ringValueBytes, err := utils.Keccak256(seedBytes)
	if err != nil {
		return nil, err
	}
	lastHash := ringValueBytes // For rest of value, recursively hash
	for len(ringValueBytes) != k.N.BitLen()/8 {
		newHash, err := utils.Keccak256(lastHash)
		if err != nil {
			return nil, err
		}
		ringValueBytes = append(ringValueBytes, newHash...)
		copy(lastHash, newHash)
	}
	panicUnless(k.N.Bytes()[0]&128 != 0, errors.New("Key must be exactly "+
		"the right length. Use a key from MakeKey, which ensures this"))
	// As described in figure 1 of https://eprint.iacr.org/2017/099.pdf,
	// zero out the top bit of ringValueBytes to ensure that it represents
	// an element of ℤ/(k.N)ℤ. (This will not sample from elements of
	// ℤ/(k.N)ℤ whose minimal representation has exactly the same bit length
	// as k.N. We lose one bit of entropy, from this.)
	ringValueBytes[0] = ringValueBytes[0] & uint8(math.Pow(2, 7)-1)
	ringValue := new(big.Int).SetBytes(ringValueBytes)
	panicUnless(ringValue.Cmp(k.N) == -1,
		errors.New("ringValue not in ℤ/((k.N)ℤ)"))
	panicUnless(PublicExponent*ringValue.BitLen() >= 2*k.N.BitLen(),
		errors.New(`(ring value)^exponent is too short to be secure.
(For a 2048-bit or longer key, this is a cryptographically impossible accident)`))
	return ringValue, nil
}

// checkKey returns an error describing any problems with k.
func checkKey(k *rsa.PrivateKey) error {
	if k.E != PublicExponent {
		return fmt.Errorf("public exponent of key must be PublicExponent")
	}
	return k.Validate()
}

// Generate returns VRF output and correctness proof from given key and seed
func Generate(k *rsa.PrivateKey, seed *big.Int) (*Proof, error) {
	if err := checkKey(k); err != nil {
		return nil, err
	}
	// Prove knowledge of the private key by "decrypting" to seed used to
	// generate Proof.Output. Nothing hidden, here, so not really decryption
	cipherText, err := seedToRingValue(seed, &k.PublicKey)
	if err != nil {
		return nil, err
	}
	decryption := decrypt(k, cipherText)
	output, err := decryptionToOutput(decryption) // Actual VRF "randomness"
	if err != nil {
		return nil, err
	}
	rv := &Proof{
		Key:        &k.PublicKey,
		Seed:       seed,
		Decryption: decryption,
		Output:     output,
	}
	ok, err := rv.Verify()
	panicUnless(err == nil && ok, multierr.Combine(
		err, errors.New("couldn't verify proof we just generated")))
	return rv, nil
}

// Verify returns true iff p is a correct proof for its output.
func (p *Proof) Verify() (bool, error) {
	output, err := decryptionToOutput(p.Decryption)
	if err != nil {
		return false, err
	}
	if output.Cmp(p.Output) != 0 { // Verify Output is hash of Decrypt
		return false, nil
	}
	// Get the value from the seed which prover should have "decrypted"
	expected, err := seedToRingValue(p.Seed, p.Key)
	if err != nil {
		return false, err
	}
	return encrypt(p.Key, p.Decryption).Cmp(expected) == 0, nil
}

// safePrime(bits) returns 2p+1 which
//
// 1. has bit-length bits,
// 2. is composite with probability less than 2^{-10000}, and
// 3. for which (p-1)/2 is composite with probability less than 2^{-5000}.
//
// https://en.wikipedia.org/wiki/Safe_prime
//
// This must use golang version at least 1.10.3. See section 4.15,
// https://eprint.iacr.org/2018/749.pdf#page=19
//
// safePrime(bits, numPrimalityChecks) returns 2p+1 satisfying the above
// constraints, except the probability it's composite is
// 2^{-2*numPrimalityChecks}. This is mostly useful for testing.
func safePrime(bitsAndNumPrimalityChecks ...uint32) *big.Int {
	panicUnless(len(bitsAndNumPrimalityChecks) >= 1 &&
		len(bitsAndNumPrimalityChecks) <= 2,
		errors.New("only one or two arguments, to safePrime"))
	bits := bitsAndNumPrimalityChecks[0]
	var numPrimalityChecks int
	if len(bitsAndNumPrimalityChecks) == 2 {
		numPrimalityChecks = int(bitsAndNumPrimalityChecks[1])
	} else {
		numPrimalityChecks = 5000
	}
	scratch1, scratch2, scratch3 := new(big.Int), new(big.Int), new(big.Int)
	for {
		// TODO(alx): Rewrite rand.Prime to quickly search for a safe
		// prime. Should be possible to speed this up a lot.
		p, err := rand.Prime(rand.Reader, int(bits)-1)
		panicUnless(err == nil, err)
		twoP := scratch2.Lsh(p, 1)
		rv := scratch1.Add(twoP, one) // 2*p+1
		// See https://en.wikipedia.org/wiki/Pocklington_primality_test
		// equations 1-3, N:=2p+1, p:=p and a:=2
		if coprime(three, rv) && // Eq. 3: a^{(N-1)/p}-1=2^2-1=4-1=3
			scratch3.Exp(two, twoP, rv).Cmp(one) == 0 { // Eq1
			// This extra primality test, combined with the
			// Pocklington test and the primality test on p, gives
			// (2^{-2*numPrimalityChecks})^2 probability that the
			// returned value is actually composite
			//
			// We can be quite sure at this point that p & rv are
			// prime, so we can afford to put a lot of work into
			// verifying that (still a tiny amount, compared to the
			// overall search.) This is potentially beneficial if
			// the source of randomness compromised.
			if rv.ProbablyPrime(numPrimalityChecks) &&
				p.ProbablyPrime((numPrimalityChecks)) {
				return rv
			}
		}
	}
}

// coprime returns true iff GCD(m,n) = 1
func coprime(m, n *big.Int) bool {
	return new(big.Int).GCD(nil, nil, m, n).Cmp(big.NewInt(1)) == 0
}

// coprimalityChecks panics if any expected coprimality does not hold.
func coprimalityChecks(p, q, pMinusOne, qMinusOne, multOrder, exp *big.Int) {
	for _, tt := range []struct {
		m   *big.Int
		n   *big.Int
		msg string
	}{
		{p, q, "p and q"},
		{new(big.Int).Mul(p, q), multOrder, "pq and (p-1)(q-1)"},
		{new(big.Int).Rsh(pMinusOne, 1), qMinusOne, "(p-1)/2, q-1"},
		{multOrder, exp, "(p-1)(q-1), exponent"},
	} {
		panicUnless(coprime(tt.m, tt.n), errors.New(tt.msg+" not coprime"))
	}
}

// MakeKey securely randomly samples a pair of large primes for an RSA modulus,
// and sets the public exponent to PublicExponent.
//
// Without an argument, defaults to KeySizeBits-sized key. Otherwise, makes a
// key of the requested size. The argument form should only be used for testing.
//
// Because this searches for safe primes, it may take a couple of minutes, even
// on a modern machine.
func MakeKey(bitsizes ...uint32) (*rsa.PrivateKey, error) {
	if len(bitsizes) > 1 {
		return nil, fmt.Errorf("specify at most one bit size")
	}
	bitsize := uint32(KeySizeBits)
	if len(bitsizes) == 1 {
		bitsize = bitsizes[0]
		fmt.Printf("Warning, generating a key of length %d. %d bits "+
			"demanded by protocol\n", bitsize, KeySizeBits)
	}
	exp := new(big.Int).SetUint64(uint64(PublicExponent))
	p := safePrime(bitsize / 2)
	pMinusOne := new(big.Int).Sub(p, one)
	q := safePrime(bitsize / 2)
	qMinusOne := new(big.Int).Sub(q, one)
	N := new(big.Int).Mul(p, q)
	panicUnless(uint32(N.BitLen()) == bitsize,
		errors.New("modulus doesn't match key size"))
	multOrder := new(big.Int).Mul(pMinusOne, qMinusOne)
	coprimalityChecks(p, q, pMinusOne, qMinusOne, multOrder, exp)
	D := new(big.Int) // Will receive "exp^{-1} mod multOrder" from GCD
	_ = new(big.Int).GCD(D, nil, exp, multOrder)
	_ = D.Mod(D, multOrder)
	dExpProd := new(big.Int).Mod(new(big.Int).Mul(D, exp), multOrder)
	panicUnless(dExpProd.Cmp(one) == 0,
		errors.New("(exp * D) ~≡ 1 mod (p-1)(q-1)"))
	rv := rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: N, E: PublicExponent},
		D:         D,
		Primes:    []*big.Int{p, q},
	}
	rv.Precompute()
	if err := rv.Validate(); err != nil {
		return nil, err
	}
	return &rv, nil
}
