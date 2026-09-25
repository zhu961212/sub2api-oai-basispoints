package protocol

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"testing"
)

func TestNumericSchemaConstraintsSurvivePrepareAndTransform(t *testing.T) {
	for _, keyword := range []string{"enum", "const"} {
		for _, test := range []struct {
			name, constraint, argument string
			wantValid                  bool
		}{
			{"large integer", "9007199254740993", "9007199254740993", true},
			{"unequal large integer", "9007199254740993", "9007199254740992", false},
			{"integer representation", "1", "1", true},
			{"decimal representation", "1", "1.0", true},
			{"exponent representation", "1", "1e0", true},
			{"unequal number", "1", "1.01", false},
		} {
			t.Run(keyword+"/"+test.name, func(t *testing.T) {
				var constraint any = json.Number(test.constraint)
				if keyword == "enum" {
					constraint = []any{constraint}
				}
				source, err := RawObject(JSONBytes(map[string]any{
					"model": "gpt-6-astra", "input": "inspect numeric value",
					"session_id": t.Name(),
					"tools": []any{map[string]any{
						"type": "function", "name": "numeric_tool",
						"parameters": map[string]any{
							"type": "object", "required": []any{"value"}, "additionalProperties": false,
							"properties": map[string]any{"value": map[string]any{keyword: constraint}},
						},
					}},
				}))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
					t.Fatal(err)
				}
				arguments := map[string]any{"value": json.Number(test.argument)}
				native := relayCompatNative(map[string]any{"tool": "numeric_tool", "args": arguments})
				body := JSONBytes(map[string]any{"id": "resp-" + t.Name(), "status": "completed", "output": []any{native}})
				_, response, changed, err := TransformResponseBody(body, source)
				if (err == nil) != test.wantValid {
					t.Fatalf("%s %s, argument %s: error = %v, want valid %v", keyword, test.constraint, test.argument, err, test.wantValid)
				}
				if !test.wantValid {
					return
				}
				if !changed {
					t.Fatal("native relay was not transformed")
				}
				output := response["output"].([]any)
				call := objectValue(output[0])
				if call["name"] != "numeric_tool" || call["arguments"] != string(JSONBytes(arguments)) {
					t.Fatalf("numeric call changed during preparation or transformation: %#v", call)
				}
			})
		}
	}
}

func TestJSONValuesEqualUsesExactNumericValue(t *testing.T) {
	for _, test := range []struct {
		name        string
		left, right any
		want        bool
	}{
		{"integer and decimal", 1, json.Number("1.0"), true},
		{"decimal and exponent", json.Number("1.0"), json.Number("1e0"), true},
		{"signed zero", json.Number("-0.00"), 0, true},
		{"fraction", json.Number("0.01"), json.Number("1e-2"), true},
		{"unequal large integers", json.Number("9007199254740993"), json.Number("9007199254740992"), false},
		{"equivalent large integer", json.Number("9007199254740993"), json.Number("9007199254740993.0"), true},
		{"huge exponent stays bounded", json.Number("1e1000000000000000000000000"), json.Number("10e999999999999999999999999"), true},
		{"number and boolean", json.Number("1"), true, false},
		{"number and string", json.Number("1"), "1", false},
		{"number and null", 0, nil, false},
		{"invalid numbers", json.Number("invalid"), json.Number("invalid"), false},
		{"nested object and array", map[string]any{"values": []any{json.Number("1e0"), map[string]any{"n": json.Number("9007199254740993.0")}}}, map[string]any{"values": []any{1, map[string]any{"n": json.Number("9007199254740993")}}}, true},
		{"nested type stays distinct", []any{map[string]any{"n": json.Number("1.0")}}, []any{map[string]any{"n": "1"}}, false},
		{"object key differs", map[string]any{"left": json.Number("1.0")}, map[string]any{"right": 1}, false},
		{"array order differs", []any{json.Number("1.0"), 2}, []any{2, 1}, false},
		{"typed arrays remain compatible", []string{"same"}, []any{"same"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := jsonValuesEqual(test.left, test.right); got != test.want {
				t.Fatalf("jsonValuesEqual(%#v, %#v) = %v, want %v", test.left, test.right, got, test.want)
			}
			if got := jsonValuesEqual(test.right, test.left); got != test.want {
				t.Fatalf("reverse equality = %v, want %v", got, test.want)
			}
		})
	}
}

func TestToolNumericValidationSurvivesPrepareAndTransform(t *testing.T) {
	for _, test := range []struct {
		name, schema, argument string
		wantValid              bool
	}{
		{"integer rejects fraction", `{"type":"integer"}`, `1.5`, false},
		{"integer accepts decimal", `{"type":"integer"}`, `1.0`, true},
		{"integer accepts exponent", `{"type":"integer"}`, `12e1`, true},
		{"integer rejects fractional exponent", `{"type":"integer"}`, `12e-1`, false},
		{"integer accepts scaled zero", `{"type":"integer"}`, `-0.00e-1000000000000000000000`, true},
		{"integer preserves large integer", `{"type":"integer"}`, `9007199254740993`, true},
		{"integer rejects rounded large fraction", `{"type":"integer"}`, `9007199254740993.1`, false},
		{"integer huge exponent", `{"type":"integer"}`, `1e1000000000000000000000`, true},
		{"integer huge negative exponent", `{"type":"integer"}`, `1e-1000000000000000000000`, false},
		{"number accepts fraction", `{"type":"number"}`, `1.5`, true},
		{"number rejects string", `{"type":"number"}`, `"1.5"`, false},
		{"minimum equal", `{"minimum":9007199254740993}`, `9007199254740993.0`, true},
		{"minimum exact rejection", `{"minimum":9007199254740993}`, `9007199254740992`, false},
		{"maximum equal", `{"maximum":9007199254740993}`, `9007199254740993`, true},
		{"maximum exact rejection", `{"maximum":9007199254740993}`, `9007199254740993.1`, false},
		{"exclusive minimum equal", `{"exclusiveMinimum":1}`, `1e0`, false},
		{"exclusive minimum greater", `{"exclusiveMinimum":1}`, `1.0000000000000001`, true},
		{"exclusive maximum equal", `{"exclusiveMaximum":1}`, `1.0`, false},
		{"exclusive maximum smaller", `{"exclusiveMaximum":1}`, `0.9999999999999999`, true},
		{"negative minimum", `{"minimum":-0.125}`, `-0.1251`, false},
		{"negative maximum", `{"maximum":-0.125}`, `-0.1249`, false},
		{"different signs", `{"minimum":0}`, `-0.001`, false},
		{"huge bounded exponent", `{"maximum":1e1000000000000000000000}`, `2e1000000000000000000000`, false},
		{"decimal multiple", `{"multipleOf":0.1}`, `0.3`, true},
		{"decimal nonmultiple", `{"multipleOf":0.1}`, `0.31`, false},
		{"scaled decimal multiple", `{"multipleOf":0.25}`, `0.5`, true},
		{"negative multiple", `{"multipleOf":0.25}`, `-1.5`, true},
		{"zero multiple", `{"multipleOf":3}`, `0`, true},
		{"zero divisor invalid", `{"multipleOf":0}`, `0`, false},
		{"negative divisor invalid", `{"multipleOf":-0.1}`, `0.3`, false},
		{"large exact nonmultiple", `{"multipleOf":2}`, `9007199254740993`, false},
		{"huge exponent multiple", `{"multipleOf":2}`, `1e1000000000000000000000`, true},
		{"huge exponent nonmultiple", `{"multipleOf":3}`, `1e1000000000000000000000`, false},
		{"subunit nonmultiple", `{"multipleOf":1}`, `1e-1000000000000000000000`, false},
		{"constraints ignore nonnumbers", `{"minimum":10,"multipleOf":2}`, `"text"`, true},
		{"union integer rejects fraction", `{"type":["integer","null"]}`, `0.5`, false},
		{"union permits null", `{"type":["integer","null"],"minimum":1}`, `null`, true},
		{"composed constraints", `{"allOf":[{"type":"integer"},{"minimum":1},{"maximum":10}]}`, `11`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			schema, err := RawObject([]byte(test.schema))
			if err != nil {
				t.Fatal(err)
			}
			source, err := RawObject(JSONBytes(map[string]any{
				"model": "gpt-6-astra", "input": "inspect numeric value", "session_id": t.Name(),
				"tools": []any{map[string]any{
					"type": "function", "name": "numeric_tool",
					"parameters": map[string]any{
						"type": "object", "required": []any{"value"}, "additionalProperties": false,
						"properties": map[string]any{"value": schema},
					},
				}},
			}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := PrepareResponsesBody(source, DefaultConfig()); err != nil {
				t.Fatal(err)
			}
			arguments := `{"value":` + test.argument + `}`
			native := relayCompatNative(map[string]any{"tool": "numeric_tool", "args": json.RawMessage(arguments)})
			body := JSONBytes(map[string]any{"id": "resp-" + t.Name(), "status": "completed", "output": []any{native}})
			_, response, changed, err := TransformResponseBody(body, source)
			if (err == nil) != test.wantValid {
				t.Fatalf("schema %s, argument %s: error = %v, want valid %v", test.schema, test.argument, err, test.wantValid)
			}
			if test.wantValid {
				if !changed {
					t.Fatal("native relay was not transformed")
				}
				call := objectValue(response["output"].([]any)[0])
				if call["name"] != "numeric_tool" || call["arguments"] != arguments {
					t.Fatalf("numeric call changed: %#v", call)
				}
			}
		})
	}
}

func TestSchemaNumbersMatchExactRationalArithmetic(t *testing.T) {
	var numbers []json.Number
	for _, coefficient := range []int{-999, -25, -10, -3, -1, 0, 1, 3, 10, 25, 999} {
		for _, exponent := range []int{-4, -1, 0, 1, 4} {
			numbers = append(numbers, json.Number(fmt.Sprintf("%de%d", coefficient, exponent)))
		}
	}
	for _, left := range numbers {
		for _, right := range numbers {
			leftNumber, leftOK := parseSchemaNumber(left)
			rightNumber, rightOK := parseSchemaNumber(right)
			if !leftOK || !rightOK {
				t.Fatalf("valid numbers rejected: %s, %s", left, right)
			}
			leftRat, _ := new(big.Rat).SetString(string(left))
			rightRat, _ := new(big.Rat).SetString(string(right))
			if got, want := leftNumber.compare(rightNumber), leftRat.Cmp(rightRat); got != want {
				t.Fatalf("compare(%s, %s) = %d, want %d", left, right, got, want)
			}
			wantMultiple := rightRat.Sign() > 0 && new(big.Rat).Quo(leftRat, rightRat).IsInt()
			if got := leftNumber.multipleOf(rightNumber); got != wantMultiple {
				t.Fatalf("multipleOf(%s, %s) = %v, want %v", left, right, got, wantMultiple)
			}
		}
	}
}

func TestNumericSchemaRejectsInvalidAndNonfiniteValues(t *testing.T) {
	for _, kind := range []string{"integer", "number"} {
		for _, value := range []any{json.Number("invalid"), math.NaN(), math.Inf(1), math.Inf(-1), true, "1"} {
			if schemaMatches(value, map[string]any{"type": kind}) {
				t.Fatalf("%s schema accepted invalid numeric value %#v", kind, value)
			}
		}
	}
}
