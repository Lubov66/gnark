// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark DO NOT EDIT

package fflonk

import (
	"errors"
	"fmt"

	"io"
	"math/big"
	"time"

	curve "github.com/consensys/gnark-crypto/ecc/bw6-761"

	"github.com/consensys/gnark-crypto/ecc/bw6-761/fflonk"

	"github.com/consensys/gnark-crypto/ecc/bw6-761/fr"
	"github.com/consensys/gnark-crypto/ecc/bw6-761/fr/hash_to_field"

	"github.com/consensys/gnark-crypto/ecc/bw6-761/kzg"

	fiatshamir "github.com/consensys/gnark-crypto/fiat-shamir"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/logger"
)

var (
	errAlgebraicRelation = errors.New("algebraic relation does not hold")
	errInvalidWitness    = errors.New("witness length is invalid")
)

func Verify(proof *Proof, vk *VerifyingKey, publicWitness fr.Vector, opts ...backend.VerifierOption) error {

	log := logger.Logger().With().Str("curve", "bn254").Str("backend", "plonk").Logger()
	start := time.Now()
	cfg, err := backend.NewVerifierConfig(opts...)
	if err != nil {
		return fmt.Errorf("create backend config: %w", err)
	}

	if len(publicWitness) != int(vk.NbPublicVariables) {
		return errInvalidWitness
	}

	// transcript to derive the challenge
	fs := fiatshamir.NewTranscript(cfg.ChallengeHash, "gamma", "beta", "alpha", "zeta")

	// The first challenge is derived using the public data: the commitments to the permutation,
	// the coefficients of the circuit, and the public inputs.
	// derive gamma from the Comm(blinded cl), Comm(blinded cr), Comm(blinded co)
	if err := bindPublicData(fs, "gamma", vk, publicWitness); err != nil {
		return err
	}
	gamma, err := deriveRandomness(fs, "gamma", &proof.LROEntangled)
	if err != nil {
		return err
	}

	// derive beta
	beta, err := deriveRandomness(fs, "beta")
	if err != nil {
		return err
	}

	// derive alpha from Com(ZEntangled), BsbComEntangled
	// /!\ Com(Z) must be added
	alphaDeps := make([]*curve.G1Affine, len(proof.BsbComEntangled)+1)
	for i := range proof.BsbComEntangled {
		alphaDeps[i] = &proof.BsbComEntangled[i]
	}
	alphaDeps[len(alphaDeps)-1] = &proof.ZEntangled
	alpha, err := deriveRandomness(fs, "alpha", alphaDeps...)
	if err != nil {
		return err
	}

	// derive zeta, the point of evaluation
	zeta, err := deriveRandomness(fs, "zeta", &proof.HEntangled)
	if err != nil {
		return err
	}

	// ζᵗ
	var zetaT fr.Element
	t := getNextDivisorRMinusOne(*vk)
	tBigInt := big.NewInt(int64(t))
	zetaT.Exp(zeta, tBigInt) // ζᵗ

	// evaluation of zhZetaT=ζᵗⁿ-1
	var zetaTPowerM, zhZetaT, lagrangeOne fr.Element
	var bExpo big.Int
	one := fr.One()
	bExpo.SetUint64(vk.Size)
	zetaTPowerM.Exp(zetaT, &bExpo)  // ζᵗⁿ
	zhZetaT.Sub(&zetaTPowerM, &one) // ζᵗⁿ-1
	lagrangeOne.Sub(&zetaT, &one).  // ζᵗ-1
					Inverse(&lagrangeOne).         // 1/(ζᵗ-1)
					Mul(&lagrangeOne, &zhZetaT).   // (ζᵗⁿ-1)/(ζᵗ-1)
					Mul(&lagrangeOne, &vk.SizeInv) // 1/n * (ζᵗⁿ-1)/(ζᵗ-1)

	// compute PI = ∑_{i<n} Lᵢ*wᵢ
	var pi fr.Element
	var accw fr.Element
	{
		// [ζᵗ-1,ζᵗ-ω,ζᵗ-ω²,..]
		dens := make([]fr.Element, len(publicWitness))
		accw.SetOne()
		for i := 0; i < len(publicWitness); i++ {
			dens[i].Sub(&zetaT, &accw)
			accw.Mul(&accw, &vk.Generator)
		}

		// [1/(ζᵗ-1),1/(ζᵗ-ω),1/(ζᵗ-ω²),..]
		invDens := fr.BatchInvert(dens)

		accw.SetOne()
		var xiLi fr.Element
		for i := 0; i < len(publicWitness); i++ {
			xiLi.Mul(&zhZetaT, &invDens[i]). // (ζᵗⁿ-1)/(ζᵗ-ωⁱ)
								Mul(&xiLi, &vk.SizeInv).      // (1/n)(ζᵗⁿ-1)/(ζᵗ-ωⁱ)
								Mul(&xiLi, &accw).            // (ωⁱ/n)(ζᵗⁿ-1)/(ζᵗ-ω^i)
								Mul(&xiLi, &publicWitness[i]) // Pi[i]*(ωⁱ/n)(ζᵗⁿ-1)/(ζᵗ-ω^i)
			accw.Mul(&accw, &vk.Generator)
			pi.Add(&pi, &xiLi)
		}

		if cfg.HashToFieldFn == nil {
			cfg.HashToFieldFn = hash_to_field.New([]byte("BSB22-Plonk"))
		}
		var hashedCmt fr.Element
		nbBuf := fr.Bytes
		if cfg.HashToFieldFn.Size() < fr.Bytes {
			nbBuf = cfg.HashToFieldFn.Size()
		}
		var wPowI, den, lagrange fr.Element
		for i := range vk.CommitmentConstraintIndexes {
			cfg.HashToFieldFn.Write(proof.BsbComEntangled[i].Marshal())
			hashBts := cfg.HashToFieldFn.Sum(nil)
			cfg.HashToFieldFn.Reset()
			hashedCmt.SetBytes(hashBts[:nbBuf])

			// Computing Lᵢ(ζ) where i=CommitmentIndex
			wPowI.Exp(vk.Generator, big.NewInt(int64(vk.NbPublicVariables)+int64(vk.CommitmentConstraintIndexes[i])))
			den.Sub(&zetaT, &wPowI) // ζᵗ-wⁱ
			lagrange.SetOne().
				Sub(&zetaTPowerM, &lagrange). // ζᵗⁿ-1
				Mul(&lagrange, &wPowI).       // wⁱ(ζᵗⁿ-1)
				Div(&lagrange, &den).         // wⁱ(ζⁿ-1)/(ζᵗ-wⁱ)
				Mul(&lagrange, &vk.SizeInv)   // wⁱ/n (ζᵗⁿ-1)/(ζᵗ-wⁱ)

			xiLi.Mul(&lagrange, &hashedCmt)
			pi.Add(&pi, &xiLi)
		}
	}

	// verify the algebraic relation
	ql := proof.BatchOpeningProof.ClaimedValues[0][setup_ql][0]
	qr := proof.BatchOpeningProof.ClaimedValues[0][setup_qr][0]
	qm := proof.BatchOpeningProof.ClaimedValues[0][setup_qm][0]
	qo := proof.BatchOpeningProof.ClaimedValues[0][setup_qo][0]
	qkIncomplete := proof.BatchOpeningProof.ClaimedValues[0][setup_qk_incomplete][0]
	s1 := proof.BatchOpeningProof.ClaimedValues[0][setup_s1][0]
	s2 := proof.BatchOpeningProof.ClaimedValues[0][setup_s2][0]
	s3 := proof.BatchOpeningProof.ClaimedValues[0][setup_s3][0]
	nbQcp := len(vk.CommitmentConstraintIndexes)
	qcp := make([]fr.Element, nbQcp)
	for i := 0; i < nbQcp; i++ {
		qcp[i] = proof.BatchOpeningProof.ClaimedValues[0][setup_s3+1+i][0]
	}
	l := proof.BatchOpeningProof.ClaimedValues[0][prover_l+nbQcp][0]
	r := proof.BatchOpeningProof.ClaimedValues[0][prover_r+nbQcp][0]
	o := proof.BatchOpeningProof.ClaimedValues[0][prover_o+nbQcp][0]
	z := proof.BatchOpeningProof.ClaimedValues[0][prover_z+nbQcp][0]
	zw := proof.BatchOpeningProof.ClaimedValues[1][0][0]
	h1 := proof.BatchOpeningProof.ClaimedValues[0][prover_h_1+nbQcp][0]
	h2 := proof.BatchOpeningProof.ClaimedValues[0][prover_h_2+nbQcp][0]
	h3 := proof.BatchOpeningProof.ClaimedValues[0][prover_h_3+nbQcp][0]
	bsb := make([]fr.Element, nbQcp)
	for i := 0; i < nbQcp; i++ {
		bsb[i] = proof.BatchOpeningProof.ClaimedValues[0][prover_h_3+1+nbQcp+i][0]
	}

	// 1 - gates
	var gates, tmp fr.Element
	gates.Mul(&ql, &l)
	tmp.Mul(&qr, &r)
	gates.Add(&gates, &tmp)
	tmp.Mul(&qm, &r).Mul(&tmp, &l)
	gates.Add(&gates, &tmp)
	tmp.Mul(&qo, &o)
	gates.Add(&gates, &tmp).Add(&gates, &qkIncomplete).Add(&gates, &pi)
	for i := 0; i < nbQcp; i++ {
		tmp.Mul(&qcp[i], &bsb[i])
		gates.Add(&gates, &tmp)
	}

	// 2 - permutation
	var permutation, tmp2, uZetaT, uuZetaT fr.Element
	permutation.Mul(&beta, &s1).Add(&permutation, &l).Add(&permutation, &gamma)
	tmp.Mul(&beta, &s2).Add(&tmp, &r).Add(&tmp, &gamma)
	permutation.Mul(&permutation, &tmp)
	tmp.Mul(&beta, &s3).Add(&tmp, &o).Add(&tmp, &gamma)
	permutation.Mul(&permutation, &tmp).Mul(&permutation, &zw)

	tmp2.Mul(&beta, &zetaT).Add(&tmp2, &l).Add(&tmp2, &gamma)
	uZetaT.Mul(&zetaT, &vk.CosetShift)
	tmp.Mul(&uZetaT, &beta).Add(&tmp, &r).Add(&tmp, &gamma)
	tmp.Mul(&tmp, &tmp2)
	uuZetaT.Mul(&uZetaT, &vk.CosetShift)
	tmp2.Mul(&uuZetaT, &beta).Add(&o, &tmp2).Add(&tmp2, &gamma)
	tmp.Mul(&tmp, &tmp2).Mul(&tmp, &z)
	permutation.Sub(&permutation, &tmp)

	// 3 - "Z starts at one"
	var startAtOne fr.Element
	startAtOne.Sub(&z, &one).Mul(&startAtOne, &lagrangeOne)

	var lhs fr.Element
	lhs.Mul(&startAtOne, &alpha).
		Add(&lhs, &permutation).
		Mul(&lhs, &alpha).
		Add(&lhs, &gates)

	// 4 - quotient H₀(ζ) + ζᵐ⁺²*H₁(ζ) + ζ²⁽ᵐ⁺²⁾*H₂(ζ)
	var quotient fr.Element
	var zetaTNPlusTwo fr.Element
	nPlusTwo := vk.Size + 2
	nPlusTwoBigInt := big.NewInt(int64(nPlusTwo))
	zetaTNPlusTwo.Exp(zetaT, nPlusTwoBigInt)
	quotient.Mul(&h3, &zetaTNPlusTwo).
		Add(&quotient, &h2).
		Mul(&quotient, &zetaTNPlusTwo).
		Add(&quotient, &h1)

	// 5 - ζⁿ-1
	var rhs fr.Element
	rhs.Mul(&zhZetaT, &quotient)

	if !rhs.Equal(&lhs) {
		return errAlgebraicRelation
	}

	// reconstruct the entangled digest and verify the opening proof
	points := make([][]fr.Element, 2)
	points[0] = make([]fr.Element, 1)
	points[1] = make([]fr.Element, 1)
	points[0][0].Set(&zeta)
	points[1][0].Mul(&zetaT, &vk.Generator)
	var foldedDigests [2]kzg.Digest
	foldedDigests[0].Set(&vk.Qpublic).
		Add(&foldedDigests[0], &proof.LROEntangled).
		Add(&foldedDigests[0], &proof.ZEntangled).
		Add(&foldedDigests[0], &proof.HEntangled)
	for i := 0; i < len(proof.BsbComEntangled); i++ {
		foldedDigests[0].Add(&foldedDigests[0], &proof.BsbComEntangled[i])
	}
	foldedDigests[1].Set(&proof.Z)
	err = fflonk.BatchVerify(proof.BatchOpeningProof, foldedDigests[:], points, cfg.KZGFoldingHash, vk.Kzg)

	log.Debug().Dur("took", time.Since(start)).Msg("verifier done")

	return err
}

func bindPublicData(fs *fiatshamir.Transcript, challenge string, vk *VerifyingKey, publicInputs []fr.Element) error {

	// coefficients
	if err := fs.Bind(challenge, vk.Qpublic.Marshal()); err != nil {
		return err
	}
	// public inputs
	for i := 0; i < len(publicInputs); i++ {
		if err := fs.Bind(challenge, publicInputs[i].Marshal()); err != nil {
			return err
		}
	}

	return nil

}

func deriveRandomness(fs *fiatshamir.Transcript, challenge string, points ...*curve.G1Affine) (fr.Element, error) {

	var buf [curve.SizeOfG1AffineUncompressed]byte
	var r fr.Element

	for _, p := range points {
		buf = p.RawBytes()
		if err := fs.Bind(challenge, buf[:]); err != nil {
			return r, err
		}
	}

	b, err := fs.ComputeChallenge(challenge)
	if err != nil {
		return r, err
	}
	r.SetBytes(b)
	return r, nil
}

// ExportSolidity not implemented for BW6-761
func (vk *VerifyingKey) ExportSolidity(w io.Writer) error {
	return errors.New("not implemented")
}
