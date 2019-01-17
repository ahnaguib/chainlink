pragma solidity 0.4.24;

/* VRF implements on-chain verification of verifiable-random-function (VRF)
 * proofs, as described in these documents:
 * https://tools.ietf.org/html/draft-goldbe-vrf-01#section-5 (spec)
 * https://eprint.iacr.org/2017/099.pdf (security proofs).
 *
 * Warnings
 * --------
 *
 * Alt-BN128 is estimated to provide about 110 bits of security against
 * discrete-log (the key security assumption used in this protocol.) See design
 * notes.
 *
 * Purpose
 * -------
 *
 * Reggie the Random Oracle (not his real job) wants to provide randomness to
 * Vera the verifier in such a way that Vera can be sure he's not making his
 * output up to suit himself. Reggie provides Vera a public key to which he
 * knows the secret key. Each time Vera provides a seed to Reggie, he gives back
 * a value which is computed completely deterministically from the seed and the
 * secret key, but which is indistinguishable from randomness to Vera.
 * Nonetheless, Vera is able to verify that Reggie's output came from her seed
 * and his secret key.
 *
 * The purpose of this contract is to perform that verification.
 *
 * Usage
 * -----
 *
 * The main entry point is isValidVRFOutput. Pass it the fields of a vrf.Proof
 * object generated by vrf.go/GenerateProof.
 *
 * Returns true iff the proof can be verified as showing that _output was
 * generated as mandated.
 *
 * See the invocation of verifyVRFProof in VRF.js, for an example.
 *
 * Design notes
 * ------------
 *
 * An elliptic curve point is generally represented in the solidity code as a
 * uint256[2], corresponding to its affine coordinates in GF(fieldSize).
 *
 * For the sake of efficiency, this implementation deviates from the spec in
 * some minor ways:
 *
 * - Keccak hash rather than SHA256. This is because it's provided natively by
 *   the EVM, and therefore costs much less gas. The impact on security should
 *   be minor.
 *
 * - Alt-BN128 curve instead of P-256. Group operations are provided for this
 *   curve as precompiled contracts, so they're much cheaper. Note, however,
 *   that this a pairing-friendly curve, which may have weaker security. See the
 *   conclusion to https://eprint.iacr.org/2016/1102.pdf , which estimates that
 *   such a curve probably provides about 110 bits of security.
 *
 *   It should be possible to abuse ECRECOVER to use secp256k1 instead, which
 *   would mitigate this weakness. See
 *   https://github.com/1Address/ecsol/blob/master/contracts/EC.sol
 *   https://ethresear.ch/t/you-can-kinda-abuse-ecrecover-to-do-ecmul-in-secp256k1-today/2384/6
 *   But note that that's estimated to cost about 32,000 gas, vs about 40,000
 *   gas as implemented here https://github.com/clearmatics/mobius/issues/69 ,
 *   and hopefully the gas cost of this implementation will drop significantly
 *   https://github.com/ethereum/EIPs/pull/1108
 *
 * - scalarFromCurve recursively hashes and takes the relevant hash bits until
 *   it finds a point less than the group order. This results in uniform
 *   sampling over the the possible values scalarFromCurve could take. The spec
 *   recommends just uing the first hash output as a uint256, which is a biased
 *   sample. See the zqHash function.
 *
 * - hashToCurve recursively hashes until it finds a curve x-ordinate. The spec
 *   recommends that the initial input should be concatenated with a nonce and
 *   then hashed, and this input should be rehashed with the nonce updated until
 *   an x-ordinate is found. Recursive hashing is slightly more efficient. The
 *   spec also recommends (https://tools.ietf.org/html/rfc8032#section-5.1.3 ,
 *   by the specification of RS2ECP) that the x-ordinate should be rejected if
 *   it is greater than the prime modulus. The modulus for Alt-BN128 is a couple
 *   of bits shorter than 2**256-1, so hashToCurve masks those bits (see
 *   zqHash.)
 *
 *   The spec also requires the y ordinate of the hashToCurve to be negated if y
 *   is odd. See http://www.secg.org/sec1-v2.pdf#page=17 . This appears to be
 *   purely for compositional purposes: They need to specify some way to
 *   determine y given x, and happened to pick that one. Here, y is determined
 *   as (x^3+3)^{(p+1)/4, instead.}
 *
 */

import "openzeppelin-solidity/contracts/math/SafeMath.sol";

contract VRF {

  using SafeMath for uint256;

  // Prime characteristic of the galois field over which the curve is defined
  // github.com/ethereum/go-ethereum/blob/1636d957/crypto/bn256/cloudflare/constants.go#L23
  // N.B.: The comments there are inaccurate. This is not actually curve BN256!!
  // (It's Alt-BN128.)
  uint256 constant fieldSize = 21888242871839275222246405745257275088696311157297823662689037894645226208583;

  uint256 constant minusOne = fieldSize.sub(1);
  uint256 constant multiplicativeGroupOrder = fieldSize.sub(1);
  uint256 constant eulersCriterionPower = multiplicativeGroupOrder.div(2);
  uint256 constant sqrtPower = fieldSize.add(1).div(4); // Works since fieldSize % 4 = 3

  // github.com/ethereum/go-ethereum/blob/1636d957/crypto/bn256/cloudflare/curve.go#L15
  uint256[2] private generator = [uint256(1), uint256(2)];
  // Number of GF(fieldSize) points on the elliptic curve
  // github.com/ethereum/go-ethereum/blob/1636d957/crypto/bn256/cloudflare/constants.go
  uint256 constant groupOrder =  21888242871839275222246405745257275088548364400416034343698204186575808495617;

  // Bits which can be used in the representation of a number less than
  // groupOrder or fieldSize (i.e., the lower 254 bits.)
  uint256 constant orderMask = (~uint256(0)) >> 2;

  uint256 constant wordLengthBytes = 0x20;

  // (_base**_exponent) % _modulus
  // Cribbed from https://medium.com/@rbkhmrcr/precompiles-solidity-e5d29bd428c4
  function bigModExp(uint256 _base, uint256 _exponent, uint256 _modulus)
    public view returns (uint256 exponentiation) {
    uint256 callResult;
    uint256[6] memory bigModExpContractInputs;
    bigModExpContractInputs[0] = wordLengthBytes;  // Length of _base
    bigModExpContractInputs[1] = wordLengthBytes;  // Length of _exponent
    bigModExpContractInputs[2] = wordLengthBytes;  // Length of _modulus
    bigModExpContractInputs[3] = _base;
    bigModExpContractInputs[4] = _exponent;
    bigModExpContractInputs[5] = _modulus;
    uint256[1] memory output;
    assembly {
      callResult :=
        staticcall(                          // Let bigmodexp use arbitrary gas
                   0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF,
                   0x05,                     // Bigmodexp contract address
                   bigModExpContractInputs,
                   0xc0,                     // Length of input segment
                   output,
                   0x20)                     // Length of output segment
      }
    if (callResult == 0) {revert("bigModExp failure!");}
    return output[0];
  }

  // _scalar*p, in the curve group. Reverts if p not on curve.
  function scalarMul(uint256[2] memory _p, uint256 _scalar)
    public view returns (uint256[2] memory xyOutput) {
    uint256 callResult;
    uint256[3] memory inputs;
    inputs[0] = _p[0];
    inputs[1] = _p[1];
    inputs[2] = _scalar;
    assembly {
      callResult := // See comments on bigModExp assembly.
        staticcall(0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF,
                   0x07, inputs, 0x60, xyOutput, 0x40)
    }
    if (callResult == 0) {revert("scalarMul failure!");}
  }

  // p1+p2, in the curve group.
  function addPoints(uint256[2] memory _p1, uint256[2] memory _p2)
    public view returns (uint256[2] memory outputs) {
    uint256 callResult;
    uint256[4] memory inputs;
    inputs[0] = _p1[0];
    inputs[1] = _p1[1];
    inputs[2] = _p2[0];
    inputs[3] = _p2[1];
    assembly {
      callResult := // See comments on bigModExp assembly.
        staticcall(0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF,
                   0x06, inputs, 0x80, outputs, 0x40)
    }
    if (callResult == 0) {revert("addPoints failure!");}
  }

  // True iff _x = a^2 for some a in base Galois field (per Euler criterion.)
  function isSquare(uint256 _x) public view returns (bool) {
    uint256 halfFermatExponentiation = bigModExp(_x, eulersCriterionPower, fieldSize);
    assert((halfFermatExponentiation == 1) || (halfFermatExponentiation == minusOne));
    return (halfFermatExponentiation == 1);
  }

  // Computes a s.t. a^2 = _x in the field. Assumes _x is a square.
  function squareRoot(uint256 _x) public view returns (uint256) {
    return bigModExp(_x, sqrtPower, fieldSize);
  }

  function ySquared(uint256 _x) public view returns (uint256) {
    // Curve equation is y^2=x^3+3. See
    // github.com/ethereum/go-ethereum/blob/1636d957/crypto/bn256/cloudflare/curve.go#L39
    return bigModExp(_x, 3, fieldSize).add(3) % fieldSize;
  }

  // True iff there is y s.t. (y^2 % fieldSize) = (x^3+3 % fieldSize)
  function isCurveXOrdinate(uint256 _x) public view returns (bool) {
    return isSquare(ySquared(_x));
  }

  // Hash _x uniformly into {0, ..., q-1}. Assumes bitlength of q is 254
  // Expects _x to *already* have the necessary entropy... If _x < q, returns _x!
  function zqHash(uint256 q, uint256 _x) public pure returns (uint256 x) {
    x = _x & orderMask;
    while (x >= q) {x = uint256(keccak256(abi.encodePacked(x))) & orderMask;}
  }

  // One-way hash function onto the curve.
  function hashToCurve(uint256[2] memory _k, uint256 _input)
    public view returns (uint256[2] memory rv) {
    bytes32 hash = keccak256(abi.encodePacked(_k[0], _k[1], _input));
    rv[0] = zqHash(fieldSize, uint256(hash));
    while  (!isCurveXOrdinate(rv[0])) {
      rv[0] = zqHash(fieldSize, uint256(keccak256(abi.encodePacked(rv[0]))));
    }
    rv[1] = squareRoot(ySquared(rv[0]));
  }

  // _c*_p1 + _s*_p2
  function linearCombination(uint256 _c, uint256[2] memory _p1, uint256 _s, uint256[2] memory _p2)
    public view returns (uint256[2] memory) {
    return addPoints(scalarMul(_p1, _c), scalarMul(_p2, _s));
  }

  // Pseudo-random number from inputs. Corresponds to vrf.go/scalarFromCurve.
  function scalarFromCurve(uint256[2] memory _hash, uint256[2] memory _pk, uint256[2] memory _gamma, uint256[2] memory _u, uint256[2] memory _v)
    public view returns (uint256 s) {
    bytes32 iHash = keccak256(abi.encodePacked(generator, _hash, _pk, _gamma, _u, _v));
    return zqHash(groupOrder, uint256(iHash));
  }

  // True if (gamma, c, s) is a correctly constructed randomness proof from _pk
  // and _seed
  function verifyVRFProof(uint256[2] memory _pk, uint256[2] memory _gamma, uint256 _c, uint256 _s, uint256 _seed)
    public view returns (bool) {
    // NB: Curve operations already check that (_pkX, _pkY), (_gammaX, _gammaY)
    // are valid curve points. No need to do that explicitly.
    uint256[2] memory u = linearCombination(_c, _pk, _s, generator);
    uint256[2] memory hash = hashToCurve(_pk, _seed);
    uint256[2] memory v = linearCombination(_c, _gamma, _s, hash);
    return (_c == scalarFromCurve(hash, _pk, _gamma, u, v));
  }

  // True if _output is correct VRF output given other parameters
  function isValidVRFOutput(uint256[2] memory _pk, uint256[2] memory _gamma, uint256 _c, uint256 _s, uint256 _seed, uint256 _output)
    public view returns (bool) {
    return verifyVRFProof(_pk, _gamma, _c, _s, _seed) && (uint256(keccak256(abi.encodePacked(_gamma))) == _output);
  }
}
