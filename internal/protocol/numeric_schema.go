package protocol

import (
	"encoding/json"
	"math/big"
	"strings"
)

type schemaNumber struct {
	coefficient string
	exponent    *big.Int
}

func parseSchemaNumber(value any) (schemaNumber, bool) {
	switch value.(type) {
	case json.Number, float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
	default:
		return schemaNumber{}, false
	}
	// Marshal also rejects invalid json.Number values and nonfinite floats.
	encoded, err := json.Marshal(value)
	if err != nil {
		return schemaNumber{}, false
	}
	coefficient, exponent := normalizedJSONNumber(json.Number(encoded))
	return schemaNumber{coefficient, exponent}, true
}

func numericSchemaMatches(value any, schema map[string]any) bool {
	var number schemaNumber
	for _, keyword := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
		constraint, exists := schema[keyword]
		if !exists {
			continue
		}
		if number.exponent == nil {
			var ok bool
			number, ok = parseSchemaNumber(value)
			if !ok {
				// Numeric keywords do not constrain other JSON types.
				return true
			}
		}
		bound, ok := parseSchemaNumber(constraint)
		if !ok {
			return false
		}
		if keyword == "multipleOf" {
			if !number.multipleOf(bound) {
				return false
			}
			continue
		}
		comparison := number.compare(bound)
		switch keyword {
		case "minimum":
			if comparison < 0 {
				return false
			}
		case "maximum":
			if comparison > 0 {
				return false
			}
		case "exclusiveMinimum":
			if comparison <= 0 {
				return false
			}
		case "exclusiveMaximum":
			if comparison >= 0 {
				return false
			}
		}
	}
	return true
}

func (number schemaNumber) sign() int {
	if number.coefficient == "0" {
		return 0
	}
	if number.coefficient[0] == '-' {
		return -1
	}
	return 1
}

// Compare decimal magnitude first, then significand digits. Neither step
// expands a power of ten, even when the supplied exponent is enormous.
func (number schemaNumber) compare(other schemaNumber) int {
	sign, otherSign := number.sign(), other.sign()
	if sign < otherSign {
		return -1
	}
	if sign > otherSign {
		return 1
	}
	if sign == 0 {
		return 0
	}
	digits := strings.TrimPrefix(number.coefficient, "-")
	otherDigits := strings.TrimPrefix(other.coefficient, "-")
	magnitude := new(big.Int).Add(number.exponent, big.NewInt(int64(len(digits))))
	otherMagnitude := new(big.Int).Add(other.exponent, big.NewInt(int64(len(otherDigits))))
	comparison := magnitude.Cmp(otherMagnitude)
	if comparison == 0 {
		// Normalized coefficients have no trailing zeroes, so lexicographic
		// comparison is equivalent to padding the shorter suffix with zeroes.
		comparison = strings.Compare(digits, otherDigits)
	}
	return sign * comparison
}

func (number schemaNumber) multipleOf(divisor schemaNumber) bool {
	if divisor.sign() <= 0 {
		return false
	}
	if number.sign() == 0 {
		return true
	}
	shift := new(big.Int).Sub(number.exponent, divisor.exponent)
	if shift.Sign() < 0 {
		// A normalized nonzero coefficient cannot be divisible by ten.
		return false
	}
	coefficient, _ := new(big.Int).SetString(number.coefficient, 10)
	modulus, _ := new(big.Int).SetString(divisor.coefficient, 10)
	// Modular exponentiation keeps intermediates bounded by the coefficient
	// sizes rather than allocating the potentially enormous expanded value.
	scale := new(big.Int).Exp(big.NewInt(10), shift, modulus)
	coefficient.Mul(coefficient, scale)
	return coefficient.Mod(coefficient, modulus).Sign() == 0
}
