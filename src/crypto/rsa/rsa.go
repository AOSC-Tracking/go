// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package rsa implements RSA encryption as specified in PKCS #1 and RFC 8017.
//
// RSA is a single, fundamental operation that is used in this package to
// implement either public-key encryption or public-key signatures.
//
// The original specification for encryption and signatures with RSA is PKCS #1
// and the terms "RSA encryption" and "RSA signatures" by default refer to
// PKCS #1 version 1.5. However, that specification has flaws and new designs
// should use version 2, usually called by just OAEP and PSS, where
// possible.
//
// Two sets of interfaces are included in this package. When a more abstract
// interface isn't necessary, there are functions for encrypting/decrypting
// with v1.5/OAEP and signing/verifying with v1.5/PSS. If one needs to abstract
// over the public key primitive, the PrivateKey type implements the
// Decrypter and Signer interfaces from the crypto package.
//
// Operations involving private keys are implemented using constant-time
// algorithms, except for [GenerateKey] and for some operations involving
// deprecated multi-prime keys.
//
// # Minimum key size
//
// [GenerateKey] returns an error if a key of less than 1024 bits is requested,
// and all Sign, Verify, Encrypt, and Decrypt methods return an error if used
// with a key smaller than 1024 bits. Such keys are insecure and should not be
// used.
//
// The `rsa1024min=0` GODEBUG setting suppresses this error, but we recommend
// doing so only in tests, if necessary. Tests can use [testing.T.Setenv] or
// include `//go:debug rsa1024min=0` in a `_test.go` source file to set it.
//
// Alternatively, see the [GenerateKey (TestKey)] example for a pregenerated
// test-only 2048-bit key.
//
// [GenerateKey (TestKey)]: #example-GenerateKey-TestKey
package rsa

import (
	"crypto"
	"crypto/internal/boring"
	"crypto/internal/boring/bbig"
	"crypto/internal/fips140/bigmod"
	"crypto/internal/fips140/rsa"
	"crypto/internal/randutil"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"internal/godebug"
	"io"
	"math"
	"math/big"
)

var bigOne = big.NewInt(1)

// A PublicKey represents the public part of an RSA key.
//
// The value of the modulus N is considered secret by this library and protected
// from leaking through timing side-channels. However, neither the value of the
// exponent E nor the precise bit size of N are similarly protected.
type PublicKey struct {
	N *big.Int // modulus
	E int      // public exponent
}

// Any methods implemented on PublicKey might need to also be implemented on
// PrivateKey, as the latter embeds the former and will expose its methods.

// Size returns the modulus size in bytes. Raw signatures and ciphertexts
// for or by this public key will have the same size.
func (pub *PublicKey) Size() int {
	return (pub.N.BitLen() + 7) / 8
}

// Equal reports whether pub and x have the same value.
func (pub *PublicKey) Equal(x crypto.PublicKey) bool {
	xx, ok := x.(*PublicKey)
	if !ok {
		return false
	}
	return bigIntEqual(pub.N, xx.N) && pub.E == xx.E
}

// OAEPOptions is an interface for passing options to OAEP decryption using the
// crypto.Decrypter interface.
type OAEPOptions struct {
	// Hash is the hash function that will be used when generating the mask.
	Hash crypto.Hash

	// MGFHash is the hash function used for MGF1.
	// If zero, Hash is used instead.
	MGFHash crypto.Hash

	// Label is an arbitrary byte string that must be equal to the value
	// used when encrypting.
	Label []byte
}

// A PrivateKey represents an RSA key
type PrivateKey struct {
	PublicKey            // public part.
	D         *big.Int   // private exponent
	Primes    []*big.Int // prime factors of N, has >= 2 elements.

	// Precomputed contains precomputed values that speed up RSA operations,
	// if available. It must be generated by calling PrivateKey.Precompute and
	// must not be modified.
	Precomputed PrecomputedValues
}

// Public returns the public key corresponding to priv.
func (priv *PrivateKey) Public() crypto.PublicKey {
	return &priv.PublicKey
}

// Equal reports whether priv and x have equivalent values. It ignores
// Precomputed values.
func (priv *PrivateKey) Equal(x crypto.PrivateKey) bool {
	xx, ok := x.(*PrivateKey)
	if !ok {
		return false
	}
	if !priv.PublicKey.Equal(&xx.PublicKey) || !bigIntEqual(priv.D, xx.D) {
		return false
	}
	if len(priv.Primes) != len(xx.Primes) {
		return false
	}
	for i := range priv.Primes {
		if !bigIntEqual(priv.Primes[i], xx.Primes[i]) {
			return false
		}
	}
	return true
}

// bigIntEqual reports whether a and b are equal leaking only their bit length
// through timing side-channels.
func bigIntEqual(a, b *big.Int) bool {
	return subtle.ConstantTimeCompare(a.Bytes(), b.Bytes()) == 1
}

// Sign signs digest with priv, reading randomness from rand. If opts is a
// *[PSSOptions] then the PSS algorithm will be used, otherwise PKCS #1 v1.5 will
// be used. digest must be the result of hashing the input message using
// opts.HashFunc().
//
// This method implements [crypto.Signer], which is an interface to support keys
// where the private part is kept in, for example, a hardware module. Common
// uses should use the Sign* functions in this package directly.
func (priv *PrivateKey) Sign(rand io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	if pssOpts, ok := opts.(*PSSOptions); ok {
		return SignPSS(rand, priv, pssOpts.Hash, digest, pssOpts)
	}

	return SignPKCS1v15(rand, priv, opts.HashFunc(), digest)
}

// Decrypt decrypts ciphertext with priv. If opts is nil or of type
// *[PKCS1v15DecryptOptions] then PKCS #1 v1.5 decryption is performed. Otherwise
// opts must have type *[OAEPOptions] and OAEP decryption is done.
func (priv *PrivateKey) Decrypt(rand io.Reader, ciphertext []byte, opts crypto.DecrypterOpts) (plaintext []byte, err error) {
	if opts == nil {
		return DecryptPKCS1v15(rand, priv, ciphertext)
	}

	switch opts := opts.(type) {
	case *OAEPOptions:
		if opts.MGFHash == 0 {
			return decryptOAEP(opts.Hash.New(), opts.Hash.New(), priv, ciphertext, opts.Label)
		} else {
			return decryptOAEP(opts.Hash.New(), opts.MGFHash.New(), priv, ciphertext, opts.Label)
		}

	case *PKCS1v15DecryptOptions:
		if l := opts.SessionKeyLen; l > 0 {
			plaintext = make([]byte, l)
			if _, err := io.ReadFull(rand, plaintext); err != nil {
				return nil, err
			}
			if err := DecryptPKCS1v15SessionKey(rand, priv, ciphertext, plaintext); err != nil {
				return nil, err
			}
			return plaintext, nil
		} else {
			return DecryptPKCS1v15(rand, priv, ciphertext)
		}

	default:
		return nil, errors.New("crypto/rsa: invalid options for Decrypt")
	}
}

type PrecomputedValues struct {
	Dp, Dq *big.Int // D mod (P-1) (or mod Q-1)
	Qinv   *big.Int // Q^-1 mod P

	// CRTValues is used for the 3rd and subsequent primes. Due to a
	// historical accident, the CRT for the first two primes is handled
	// differently in PKCS #1 and interoperability is sufficiently
	// important that we mirror this.
	//
	// Deprecated: These values are still filled in by Precompute for
	// backwards compatibility but are not used. Multi-prime RSA is very rare,
	// and is implemented by this package without CRT optimizations to limit
	// complexity.
	CRTValues []CRTValue

	fips *rsa.PrivateKey
}

// CRTValue contains the precomputed Chinese remainder theorem values.
type CRTValue struct {
	Exp   *big.Int // D mod (prime-1).
	Coeff *big.Int // R·Coeff ≡ 1 mod Prime.
	R     *big.Int // product of primes prior to this (inc p and q).
}

// Validate performs basic sanity checks on the key.
// It returns nil if the key is valid, or else an error describing a problem.
//
// It runs faster on valid keys if run after [Precompute].
func (priv *PrivateKey) Validate() error {
	// We can operate on keys based on d alone, but it isn't possible to encode
	// with [crypto/x509.MarshalPKCS1PrivateKey], which unfortunately doesn't
	// return an error.
	if len(priv.Primes) < 2 {
		return errors.New("crypto/rsa: missing primes")
	}
	// If Precomputed.fips is set, then the key has been validated by
	// [rsa.NewPrivateKey] or [rsa.NewPrivateKeyWithoutCRT].
	if priv.Precomputed.fips != nil {
		return nil
	}
	_, err := priv.precompute()
	return err
}

// rsa1024min is a GODEBUG that re-enables weak RSA keys if set to "0".
// See https://go.dev/issue/68762.
var rsa1024min = godebug.New("rsa1024min")

func checkKeySize(size int) error {
	if size >= 1024 {
		return nil
	}
	if rsa1024min.Value() == "0" {
		rsa1024min.IncNonDefault()
		return nil
	}
	return fmt.Errorf("crypto/rsa: %d-bit keys are insecure (see https://go.dev/pkg/crypto/rsa#hdr-Minimum_key_size)", size)
}

func checkPublicKeySize(k *PublicKey) error {
	if k.N == nil {
		return errors.New("crypto/rsa: missing public modulus")
	}
	return checkKeySize(k.N.BitLen())
}

// GenerateKey generates a random RSA private key of the given bit size.
//
// If bits is less than 1024, [GenerateKey] returns an error. See the "[Minimum
// key size]" section for further details.
//
// Most applications should use [crypto/rand.Reader] as rand. Note that the
// returned key does not depend deterministically on the bytes read from rand,
// and may change between calls and/or between versions.
//
// [Minimum key size]: #hdr-Minimum_key_size
func GenerateKey(random io.Reader, bits int) (*PrivateKey, error) {
	if err := checkKeySize(bits); err != nil {
		return nil, err
	}
	return GenerateMultiPrimeKey(random, 2, bits)
}

// GenerateMultiPrimeKey generates a multi-prime RSA keypair of the given bit
// size and the given random source.
//
// Table 1 in "[On the Security of Multi-prime RSA]" suggests maximum numbers of
// primes for a given bit size.
//
// Although the public keys are compatible (actually, indistinguishable) from
// the 2-prime case, the private keys are not. Thus it may not be possible to
// export multi-prime private keys in certain formats or to subsequently import
// them into other code.
//
// This package does not implement CRT optimizations for multi-prime RSA, so the
// keys with more than two primes will have worse performance.
//
// Deprecated: The use of this function with a number of primes different from
// two is not recommended for the above security, compatibility, and performance
// reasons. Use [GenerateKey] instead.
//
// [On the Security of Multi-prime RSA]: http://www.cacr.math.uwaterloo.ca/techreports/2006/cacr2006-16.pdf
func GenerateMultiPrimeKey(random io.Reader, nprimes int, bits int) (*PrivateKey, error) {
	randutil.MaybeReadByte(random)

	if boring.Enabled && random == boring.RandReader && nprimes == 2 &&
		(bits == 2048 || bits == 3072 || bits == 4096) {
		bN, bE, bD, bP, bQ, bDp, bDq, bQinv, err := boring.GenerateKeyRSA(bits)
		if err != nil {
			return nil, err
		}
		N := bbig.Dec(bN)
		E := bbig.Dec(bE)
		D := bbig.Dec(bD)
		P := bbig.Dec(bP)
		Q := bbig.Dec(bQ)
		Dp := bbig.Dec(bDp)
		Dq := bbig.Dec(bDq)
		Qinv := bbig.Dec(bQinv)
		e64 := E.Int64()
		if !E.IsInt64() || int64(int(e64)) != e64 {
			return nil, errors.New("crypto/rsa: generated key exponent too large")
		}

		key := &PrivateKey{
			PublicKey: PublicKey{
				N: N,
				E: int(e64),
			},
			D:      D,
			Primes: []*big.Int{P, Q},
			Precomputed: PrecomputedValues{
				Dp:        Dp,
				Dq:        Dq,
				Qinv:      Qinv,
				CRTValues: make([]CRTValue, 0), // non-nil, to match Precompute
			},
		}
		return key, nil
	}

	priv := new(PrivateKey)
	priv.E = 65537

	if nprimes < 2 {
		return nil, errors.New("crypto/rsa: GenerateMultiPrimeKey: nprimes must be >= 2")
	}

	if bits < 64 {
		primeLimit := float64(uint64(1) << uint(bits/nprimes))
		// pi approximates the number of primes less than primeLimit
		pi := primeLimit / (math.Log(primeLimit) - 1)
		// Generated primes start with 11 (in binary) so we can only
		// use a quarter of them.
		pi /= 4
		// Use a factor of two to ensure that key generation terminates
		// in a reasonable amount of time.
		pi /= 2
		if pi <= float64(nprimes) {
			return nil, errors.New("crypto/rsa: too few primes of given length to generate an RSA key")
		}
	}

	primes := make([]*big.Int, nprimes)

NextSetOfPrimes:
	for {
		todo := bits
		// crypto/rand should set the top two bits in each prime.
		// Thus each prime has the form
		//   p_i = 2^bitlen(p_i) × 0.11... (in base 2).
		// And the product is:
		//   P = 2^todo × α
		// where α is the product of nprimes numbers of the form 0.11...
		//
		// If α < 1/2 (which can happen for nprimes > 2), we need to
		// shift todo to compensate for lost bits: the mean value of 0.11...
		// is 7/8, so todo + shift - nprimes * log2(7/8) ~= bits - 1/2
		// will give good results.
		if nprimes >= 7 {
			todo += (nprimes - 2) / 5
		}
		for i := 0; i < nprimes; i++ {
			var err error
			primes[i], err = rand.Prime(random, todo/(nprimes-i))
			if err != nil {
				return nil, err
			}
			todo -= primes[i].BitLen()
		}

		// Make sure that primes is pairwise unequal.
		for i, prime := range primes {
			for j := 0; j < i; j++ {
				if prime.Cmp(primes[j]) == 0 {
					continue NextSetOfPrimes
				}
			}
		}

		n := new(big.Int).Set(bigOne)
		totient := new(big.Int).Set(bigOne)
		pminus1 := new(big.Int)
		for _, prime := range primes {
			n.Mul(n, prime)
			pminus1.Sub(prime, bigOne)
			totient.Mul(totient, pminus1)
		}
		if n.BitLen() != bits {
			// This should never happen for nprimes == 2 because
			// crypto/rand should set the top two bits in each prime.
			// For nprimes > 2 we hope it does not happen often.
			continue NextSetOfPrimes
		}

		priv.D = new(big.Int)
		e := big.NewInt(int64(priv.E))
		ok := priv.D.ModInverse(e, totient)

		if ok != nil {
			priv.Primes = primes
			priv.N = n
			break
		}
	}

	priv.Precompute()
	if err := priv.Validate(); err != nil {
		return nil, err
	}

	return priv, nil
}

// ErrMessageTooLong is returned when attempting to encrypt or sign a message
// which is too large for the size of the key. When using [SignPSS], this can also
// be returned if the size of the salt is too large.
var ErrMessageTooLong = errors.New("crypto/rsa: message too long for RSA key size")

// ErrDecryption represents a failure to decrypt a message.
// It is deliberately vague to avoid adaptive attacks.
var ErrDecryption = errors.New("crypto/rsa: decryption error")

// ErrVerification represents a failure to verify a signature.
// It is deliberately vague to avoid adaptive attacks.
var ErrVerification = errors.New("crypto/rsa: verification error")

// Precompute performs some calculations that speed up private key operations
// in the future. It is safe to run on non-validated private keys.
func (priv *PrivateKey) Precompute() {
	if priv.Precomputed.fips != nil {
		return
	}

	precomputed, err := priv.precompute()
	if err != nil {
		// We don't have a way to report errors, so just leave the key
		// unmodified. Validate will re-run precompute.
		return
	}
	priv.Precomputed = precomputed
}

func (priv *PrivateKey) precompute() (PrecomputedValues, error) {
	var precomputed PrecomputedValues

	if priv.N == nil {
		return precomputed, errors.New("crypto/rsa: missing public modulus")
	}
	if priv.D == nil {
		return precomputed, errors.New("crypto/rsa: missing private exponent")
	}
	if len(priv.Primes) != 2 {
		return priv.precomputeLegacy()
	}
	if priv.Primes[0] == nil {
		return precomputed, errors.New("crypto/rsa: prime P is nil")
	}
	if priv.Primes[1] == nil {
		return precomputed, errors.New("crypto/rsa: prime Q is nil")
	}

	k, err := rsa.NewPrivateKey(priv.N.Bytes(), priv.E, priv.D.Bytes(),
		priv.Primes[0].Bytes(), priv.Primes[1].Bytes())
	if err != nil {
		return precomputed, err
	}

	precomputed.fips = k
	_, _, _, _, _, dP, dQ, qInv := k.Export()
	precomputed.Dp = new(big.Int).SetBytes(dP)
	precomputed.Dq = new(big.Int).SetBytes(dQ)
	precomputed.Qinv = new(big.Int).SetBytes(qInv)
	precomputed.CRTValues = make([]CRTValue, 0)
	return precomputed, nil
}

func (priv *PrivateKey) precomputeLegacy() (PrecomputedValues, error) {
	var precomputed PrecomputedValues

	k, err := rsa.NewPrivateKeyWithoutCRT(priv.N.Bytes(), priv.E, priv.D.Bytes())
	if err != nil {
		return precomputed, err
	}
	precomputed.fips = k

	if len(priv.Primes) < 2 {
		return precomputed, nil
	}

	// Ensure the Mod and ModInverse calls below don't panic.
	for _, prime := range priv.Primes {
		if prime == nil {
			return precomputed, errors.New("crypto/rsa: prime factor is nil")
		}
		if prime.Cmp(bigOne) <= 0 {
			return precomputed, errors.New("crypto/rsa: prime factor is <= 1")
		}
	}

	precomputed.Dp = new(big.Int).Sub(priv.Primes[0], bigOne)
	precomputed.Dp.Mod(priv.D, precomputed.Dp)

	precomputed.Dq = new(big.Int).Sub(priv.Primes[1], bigOne)
	precomputed.Dq.Mod(priv.D, precomputed.Dq)

	precomputed.Qinv = new(big.Int).ModInverse(priv.Primes[1], priv.Primes[0])
	if precomputed.Qinv == nil {
		return precomputed, errors.New("crypto/rsa: prime factors are not relatively prime")
	}

	r := new(big.Int).Mul(priv.Primes[0], priv.Primes[1])
	precomputed.CRTValues = make([]CRTValue, len(priv.Primes)-2)
	for i := 2; i < len(priv.Primes); i++ {
		prime := priv.Primes[i]
		values := &precomputed.CRTValues[i-2]

		values.Exp = new(big.Int).Sub(prime, bigOne)
		values.Exp.Mod(priv.D, values.Exp)

		values.R = new(big.Int).Set(r)
		values.Coeff = new(big.Int).ModInverse(r, prime)
		if values.Coeff == nil {
			return precomputed, errors.New("crypto/rsa: prime factors are not relatively prime")
		}

		r.Mul(r, prime)
	}

	return precomputed, nil
}

func fipsPublicKey(pub *PublicKey) (*rsa.PublicKey, error) {
	N, err := bigmod.NewModulus(pub.N.Bytes())
	if err != nil {
		return nil, err
	}
	return &rsa.PublicKey{N: N, E: pub.E}, nil
}

func fipsPrivateKey(priv *PrivateKey) (*rsa.PrivateKey, error) {
	if priv.Precomputed.fips != nil {
		return priv.Precomputed.fips, nil
	}
	precomputed, err := priv.precompute()
	if err != nil {
		return nil, err
	}
	return precomputed.fips, nil
}
