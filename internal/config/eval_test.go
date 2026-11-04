package config

import (
	"errors"
	"math"
	"testing"
)

func TestEval_Validate(t *testing.T) {
	ok := Eval{QPM: DefaultEvalQPM, LLMCallsPerTrial: DefaultEvalLLMCallsPerTrial, VoidInfraErrorRate: DefaultEvalVoidInfraErrorRate}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]Eval{
		"qpm 0":     {QPM: 0, LLMCallsPerTrial: 3, VoidInfraErrorRate: 0.05},
		"calls 0":   {QPM: 5, LLMCallsPerTrial: 0, VoidInfraErrorRate: 0.05},
		"rate < 0":  {QPM: 5, LLMCallsPerTrial: 3, VoidInfraErrorRate: -0.1},
		"rate >= 1": {QPM: 5, LLMCallsPerTrial: 3, VoidInfraErrorRate: 1},
		"rate 0":    {QPM: 5, LLMCallsPerTrial: 3, VoidInfraErrorRate: 0},
		"rate > 5%": {QPM: 5, LLMCallsPerTrial: 3, VoidInfraErrorRate: 0.051},
		"rate NaN":  {QPM: 5, LLMCallsPerTrial: 3, VoidInfraErrorRate: math.NaN()},
	} {
		if err := e.Validate(); !errors.Is(err, ErrBadEval) {
			t.Errorf("%s: err = %v, want ErrBadEval", name, err)
		}
	}
}
