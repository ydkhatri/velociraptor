package evaluator

import (
	"context"
	"fmt"

	"github.com/Velocidex/ordereddict"
	"github.com/Velocidex/sigma-go"
	"www.velocidex.com/golang/vfilter"
	"www.velocidex.com/golang/vfilter/types"
)

type Result struct {
	// whether this event matches the Sigma rule
	Match bool `json:"match,omitempty"`

	// For each Search, whether it matched the event
	SearchResults map[string]bool `json:"search_results,omitempty"`

	// For each Condition, whether it matched the event
	ConditionResults []bool `json:"condition_results,omitempty"`

	CorrelationHits []*Event `json:"correlation_hits,omitempty"`
}

type VQLRuleEvaluator struct {
	sigma.Rule
	scope types.Scope

	// If the rule specifies a VQL transformer we use that to
	// transform the event.
	lambda      *vfilter.Lambda
	lambda_args *ordereddict.Dict

	fieldmappings []FieldMappingRecord

	// If this rule has a correlator, then forward the match to the
	// correlator.
	Correlator *SigmaCorrelator `json:"correlator,omitempty" yaml:"correlator,omitempty"`
}

type FieldMappingRecord struct {
	Name   string
	Lambda *vfilter.Lambda
}

func NewVQLRuleEvaluator(
	scope types.Scope,
	rule sigma.Rule,
	fieldmappings []FieldMappingRecord) *VQLRuleEvaluator {
	result := &VQLRuleEvaluator{
		scope:         scope,
		Rule:          rule,
		fieldmappings: fieldmappings,
	}
	return result
}

// TODO: Not supported yet
func (self *VQLRuleEvaluator) evaluateAggregationExpression(
	ctx context.Context, conditionIndex int,
	aggregation sigma.AggregationExpr, event *Event) (bool, error) {
	return false, nil
}

func (self *VQLRuleEvaluator) MaybeEnrichWithVQL(
	ctx context.Context, scope types.Scope, event *Event) *Event {
	if self.lambda != nil {
		new_event := NewEvent(event.Copy())
		subscope := scope.Copy().AppendVars(self.lambda_args)
		defer subscope.Close()

		row := self.lambda.Reduce(ctx, subscope, []vfilter.Any{event})

		// Merge the row into the event. This allows the VQL lambda to
		// set any field.
		for _, k := range scope.GetMembers(row) {
			v, _ := scope.Associative(row, k)
			new_event.Set(k, v)
		}
		return new_event
	}

	return event
}

func (self *VQLRuleEvaluator) Match(ctx context.Context,
	scope types.Scope, event *Event) (*Result, error) {

	subscope := scope.Copy().AppendVars(
		ordereddict.NewDict().
			Set("Event", event).
			Set("Rule", self.Rule))
	defer subscope.Close()

	result := Result{
		Match:            false,
		SearchResults:    map[string]bool{},
		ConditionResults: make([]bool, len(self.Detection.Conditions)),
	}

	// TODO: This needs to be done lazily so conditions do not need to
	// be evaluated needlessly.
	for identifier, search := range self.Detection.Searches {
		var err error

		eval_result, err := self.evaluateSearch(ctx, subscope, search, event)
		if err != nil {
			return nil, fmt.Errorf("error evaluating search %s: %w", identifier, err)
		}
		result.SearchResults[identifier] = eval_result
	}

	for conditionIndex, condition := range self.Detection.Conditions {
		searchMatches := self.evaluateSearchExpression(condition.Search, result.SearchResults)

		switch {
		// Event didn't match filters
		case !searchMatches:
			result.ConditionResults[conditionIndex] = false
			continue

		// Simple query without any aggregation
		case searchMatches && condition.Aggregation == nil:
			result.ConditionResults[conditionIndex] = true
			result.Match = true
			continue // need to continue in case other conditions contain aggregations that need to be evaluated

		// Search expression matched but still need to see if the aggregation returns true
		case searchMatches && condition.Aggregation != nil:
			aggregationMatches, err := self.evaluateAggregationExpression(ctx, conditionIndex, condition.Aggregation, event)
			if err != nil {
				return nil, err
			}
			if aggregationMatches {
				result.Match = true
				result.ConditionResults[conditionIndex] = true
			}
			continue
		}
	}

	// If we get here the base rule would have matched - if there is a
	// correlator tell it about it.
	if result.Match && self.Correlator != nil {
		return self.Correlator.Match(ctx, scope, self, event)
	}

	return &result, nil
}
