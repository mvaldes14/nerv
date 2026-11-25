package investigation

import (
	"context"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/mvaldes14/nerv/internal/telemetry"
)

type agentLoopStart struct {
	StartedAt   time.Time        `json:"started_at"`
	Budget      AgentBudget      `json:"budget"`
	Definitions []ToolDefinition `json:"definitions"`
}

type agentModelStep func(context.Context, int, time.Duration, AgentRequest) (AgentResponse, error)
type agentToolStep func(context.Context, int, time.Duration, ToolCall) (<-chan dbos.StepOutcome[ToolResult], error)

func (s *Service) runAgentLoop(ctx dbos.Context, workflowSpan context.Context, client AgentLLM, req Request, decision Decision, trigger Evidence) (Generation, error) {
	configuredDefinitions := []ToolDefinition(nil)
	if s.tools != nil {
		configuredDefinitions = s.tools.Definitions()
	}
	state, err := dbos.RunAsStep(ctx, func(context.Context) (agentLoopStart, error) {
		return agentLoopStart{StartedAt: time.Now().UTC(), Budget: s.agentBudget, Definitions: configuredDefinitions}, nil
	}, dbos.WithStepName("agent-budget-start"))
	if err != nil {
		return Generation{}, err
	}
	modelStep := func(parent context.Context, round int, timeout time.Duration, request AgentRequest) (AgentResponse, error) {
		return dbos.RunAsStep(ctx, func(stepCtx context.Context) (AgentResponse, error) {
			modelCtx, cancel := context.WithTimeout(telemetry.WithSpan(stepCtx, parent), timeout)
			defer cancel()
			return client.Complete(modelCtx, request)
		}, dbos.WithStepName(fmt.Sprintf("agent-model-round-%02d", round)))
	}
	toolStep := func(parent context.Context, ordinal int, timeout time.Duration, call ToolCall) (<-chan dbos.StepOutcome[ToolResult], error) {
		return dbos.Go(ctx, func(stepCtx context.Context) (ToolResult, error) {
			toolCtx, cancel := context.WithTimeout(telemetry.WithSpan(stepCtx, parent), timeout)
			defer cancel()
			return s.tools.Execute(toolCtx, call), nil
		}, dbos.WithStepName(fmt.Sprintf("execute-tool-%02d", ordinal)))
	}
	return s.runAgentLoopWithSteps(workflowSpan, req, decision, trigger, state, modelStep, toolStep)
}

func (s *Service) runAgentLoopWithSteps(workflowSpan context.Context, req Request, decision Decision, trigger Evidence, state agentLoopStart, modelStep agentModelStep, toolStep agentToolStep) (generation Generation, loopErr error) {
	startedAt, limits, definitions := state.StartedAt, state.Budget, state.Definitions
	agentCtx, agentSpan := telemetry.StartAgent(workflowSpan, s.agentProvider, s.agentModel, limits.MaxRounds, limits.MaxToolCalls, limits.MaxTokens)
	messages := []AgentMessage{
		{Role: "system", Content: agentSystemPrompt()},
		{Role: "user", Content: agentUserPrompt(req, decision, trigger)},
	}
	budget := BudgetMetadata{
		MaxRounds:      limits.MaxRounds,
		MaxToolCalls:   limits.MaxToolCalls,
		MaxTokens:      limits.MaxTokens,
		TimeoutMillis:  limits.Timeout.Milliseconds(),
		CostAccounting: "unknown_disabled",
	}
	defer func() {
		agentSpan.AgentResult(budget.RoundsUsed, budget.ToolCallsUsed, budget.TotalTokens, budget.StopReason)
		agentSpan.End(loopErr, "")
	}()
	var evidence []Evidence
	evidenceBytes := 0
	var toolsUsed []string
	seenTools := map[string]bool{}
	allowedTools := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		allowedTools[definition.Function.Name] = true
	}
	seenCallIDs := map[string]bool{}
	seenCalls := map[string]bool{}
	lastProvider, lastModel := s.agentProvider, s.agentModel
	lastInputTokens := 0

	stop := func(reason string) Generation {
		budget.StopReason = reason
		cost, accounting := calculateCost(limits, budget.InputTokens, budget.OutputTokens)
		budget.CostUSD, budget.CostAccounting = cost, accounting
		return Generation{
			Answer:            fmt.Sprintf("Investigation stopped because agent budget was exhausted (%s). Evidence collected before the stop remains available below.", reason),
			Confidence:        "unknown",
			Uncertainty:       []string{"Agent did not reach a complete structured finding before budget exhaustion."},
			RecommendedAction: "Review executed evidence and narrow the question or increase the relevant bounded budget after operator approval.",
			ToolsUsed:         toolsUsed,
			Evidence:          evidence,
			Provider:          lastProvider,
			Model:             lastModel,
			Budget:            budget,
		}
	}

	for round := 1; round <= limits.MaxRounds; round++ {
		if time.Since(startedAt) >= limits.Timeout {
			return stop("time_limit"), nil
		}
		if budget.TotalTokens >= limits.MaxTokens {
			return stop("token_limit"), nil
		}
		if cost, accounting := calculateCost(limits, budget.InputTokens, budget.OutputTokens); accounting == "known" {
			budget.CostUSD, budget.CostAccounting = cost, accounting
			if limits.MaxCostUSD > 0 && *cost >= limits.MaxCostUSD {
				return stop("cost_limit"), nil
			}
		}

		remaining := limits.Timeout - time.Since(startedAt)
		callTimeout := min(remaining, limits.ModelTimeout)
		if callTimeout <= 0 {
			return stop("time_limit"), nil
		}
		remainingTokens := limits.MaxTokens - budget.TotalTokens
		maxOutputTokens := remainingTokens - lastInputTokens
		if maxOutputTokens <= 0 {
			return stop("token_limit"), nil
		}
		tools := definitions
		toolChoice := any("auto")
		toolsAvailable := round < limits.MaxRounds && budget.ToolCallsUsed < limits.MaxToolCalls
		if !toolsAvailable {
			tools = nil
			toolChoice = "none"
			messages = append(messages, AgentMessage{Role: "user", Content: "Tool execution is finished. Return the required structured finding now using only collected evidence. State capability gaps and uncertainty explicitly; do not request more tools."})
		}
		response, err := modelStep(agentCtx, round, callTimeout, AgentRequest{
			Messages:        messages,
			Tools:           tools,
			ToolChoice:      toolChoice,
			ResponseFormat:  finalResponseFormat(),
			MaxOutputTokens: min(maxOutputTokens, 2048),
		})
		if err != nil {
			return Generation{}, err
		}
		budget.RoundsUsed = round
		budget.InputTokens += response.Usage.InputTokens
		budget.OutputTokens += response.Usage.OutputTokens
		lastInputTokens = response.Usage.InputTokens
		budget.ModelAttempts += max(response.Attempts, 1)
		budget.FallbackUsed = budget.FallbackUsed || response.FallbackUsed
		lastProvider, lastModel = response.Provider, response.Model
		used := response.Usage.TotalTokens
		if used <= 0 {
			used = response.Usage.InputTokens + response.Usage.OutputTokens
		}
		if used <= 0 {
			return stop("token_usage_unavailable"), nil
		}
		budget.TotalTokens += used
		budget.CostUSD, budget.CostAccounting = calculateCost(limits, budget.InputTokens, budget.OutputTokens)
		if budget.TotalTokens > limits.MaxTokens {
			return stop("token_limit"), nil
		}
		if limits.MaxCostUSD > 0 && budget.CostUSD != nil && *budget.CostUSD > limits.MaxCostUSD {
			return stop("cost_limit"), nil
		}
		messages = append(messages, response.Message)

		if len(response.Message.ToolCalls) == 0 {
			answer, parseErr := parseStructuredAnswer(response.Message.Content)
			if parseErr == nil {
				cost, accounting := calculateCost(limits, budget.InputTokens, budget.OutputTokens)
				budget.CostUSD, budget.CostAccounting = cost, accounting
				return Generation{
					Answer:            answer.Answer,
					Confidence:        answer.Confidence,
					Uncertainty:       answer.Uncertainty,
					RecommendedAction: answer.RecommendedAction,
					ToolsUsed:         toolsUsed,
					Evidence:          evidence,
					Provider:          lastProvider,
					Model:             lastModel,
					Budget:            budget,
				}, nil
			}
			messages = append(messages, AgentMessage{Role: "user", Content: "Previous response failed required structured JSON validation. Return only a valid structured finding or request approved tools."})
			continue
		}

		if budget.TotalTokens >= limits.MaxTokens {
			return stop("token_limit"), nil
		}
		calls := response.Message.ToolCalls
		if len(calls) > 0 && !toolsAvailable {
			return stop("tool_call_after_tools_disabled"), nil
		}
		if len(calls) > 0 && toolStep == nil {
			return Generation{}, fmt.Errorf("tool step is required when the model requests tools")
		}
		for base := 0; base < len(calls); base += max(limits.ToolConcurrency, 1) {
			end := min(base+max(limits.ToolConcurrency, 1), len(calls))
			type pending struct {
				call   ToolCall
				ch     <-chan dbos.StepOutcome[ToolResult]
				result *ToolResult
			}
			batch := make([]pending, 0, end-base)
			for index := base; index < end; index++ {
				call := calls[index]
				if call.ID == "" {
					call.ID = fmt.Sprintf("round-%02d-call-%02d", round, index+1)
				}
				if budget.ToolCallsUsed >= limits.MaxToolCalls {
					result := ToolResult{CallID: call.ID, Name: call.Function.Name, Error: "tool call budget exhausted", Content: `{"error":"tool call budget exhausted"}`}
					batch = append(batch, pending{call: call, result: &result})
					continue
				}
				budget.ToolCallsUsed++
				ordinal := budget.ToolCallsUsed
				if rejection := validateToolCallEnvelope(call, seenCallIDs, seenCalls); rejection != "" {
					result := toolCallError(call, rejection)
					batch = append(batch, pending{call: call, result: &result})
					continue
				}
				if !allowedTools[call.Function.Name] {
					result := toolCallError(call, "unknown or unapproved tool rejected")
					batch = append(batch, pending{call: call, result: &result})
					continue
				}
				if s.tools == nil {
					result := ToolResult{CallID: call.ID, Name: call.Function.Name, Error: "Kubernetes tools unavailable", Content: `{"error":"Kubernetes tools unavailable"}`}
					batch = append(batch, pending{call: call, result: &result})
					continue
				}
				toolCall := call
				toolTimeout := min(limits.ToolTimeout, limits.Timeout-time.Since(startedAt))
				if toolTimeout <= 0 {
					return stop("time_limit"), nil
				}
				ch, runErr := toolStep(agentCtx, ordinal, toolTimeout, toolCall)
				if runErr != nil {
					return Generation{}, runErr
				}
				batch = append(batch, pending{call: call, ch: ch})
			}
			for i := range batch {
				item := &batch[i]
				var result ToolResult
				if item.result != nil {
					result = *item.result
				} else {
					outcome := <-item.ch
					if outcome.Err != nil {
						return Generation{}, outcome.Err
					}
					result = outcome.Result
				}
				if result.CallID == "" {
					result.CallID = item.call.ID
				}
				if result.Name == "" {
					result.Name = item.call.Function.Name
				}
				remainingEvidence := limits.MaxEvidenceBytes - evidenceBytes
				evidenceContent, usedBytes := boundedToolContent(result.Content, remainingEvidence)
				messages = append(messages, AgentMessage{Role: "tool", ToolCallID: result.CallID, Name: result.Name, Content: evidenceContent})
				if usedBytes > 0 {
					evidenceBytes += usedBytes
					evidence = append(evidence, Evidence{Source: "tool." + result.Name, Summary: evidenceContent})
				}
				if result.Error == "" && !seenTools[result.Name] {
					seenTools[result.Name] = true
					toolsUsed = append(toolsUsed, result.Name)
				}
			}
		}
	}
	return stop("round_limit"), nil
}
