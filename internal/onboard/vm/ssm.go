package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SSMAPI is the part of the SSM client the runner uses.
type SSMAPI interface {
	SendCommand(ctx context.Context, in *ssm.SendCommandInput, opts ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(ctx context.Context, in *ssm.GetCommandInvocationInput, opts ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

// The document parameters and step names. Infra's documents
// (terraform/platform/aws/onboarding) define the same.
const (
	// HopCharlie-Inspect: publicKey, maxChunk. Steps InspectStep, then
	// ChunkStep(1) .. ChunkStep(InspectChunks-1), then a cleanup step.
	InspectStep   = "inspect"
	InspectChunks = 10
	// MaxChunk is the maxChunk the platform passes: under the 24,000
	// characters GetCommandInvocation returns of a step.
	MaxChunk = 20000
	// HopCharlie-Connect and HopCharlie-Disconnect have one step each.
	ConnectStep    = "connect"
	DisconnectStep = "disconnect"
)

// ChunkStep is the name of the inspect document's step that prints chunk i.
func ChunkStep(i int) string { return fmt.Sprintf("chunk%d", i) }

// StepResult is what one step of a command printed.
type StepResult struct {
	Status   string // SSM's: Success, Failed, TimedOut, Cancelled, ...
	ExitCode int32
	Stdout   string
	// Stderr is the script's own message, for the operator; the scripts
	// never print values to it.
	Stderr string
}

// Failed reports whether the step did not succeed.
func (r StepResult) Failed() bool { return r.Status != string(ssmtypes.CommandInvocationStatusSuccess) }

// Runner sends one fixed document to one instance and reads its steps.
type Runner struct {
	SSM SSMAPI
	// Poll is the wait between reads of a running step (default 2s);
	// Timeout bounds one Run (default 10 minutes).
	Poll    time.Duration
	Timeout time.Duration
}

// Send starts document on instance with params and returns the command id.
// The error never contains the parameters (an auth key is one).
func (r *Runner) Send(ctx context.Context, instance, document string, params map[string]string) (string, error) {
	p := make(map[string][]string, len(params))
	for k, v := range params {
		p[k] = []string{v}
	}
	out, err := r.SSM.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName:   aws.String(document),
		InstanceIds:    []string{instance},
		Parameters:     p,
		TimeoutSeconds: aws.Int32(600),
		Comment:        aws.String("hopcharlie onboarding (ADR 0042)"),
	})
	if err != nil {
		return "", fmt.Errorf("ssm: send %s to %s: %s", document, instance, apiErrorCode(err))
	}
	if out.Command == nil || aws.ToString(out.Command.CommandId) == "" {
		return "", fmt.Errorf("ssm: send %s to %s: no command id", document, instance)
	}
	return aws.ToString(out.Command.CommandId), nil
}

// Wait reads one step of a command until it is done. step "" is the
// command's only step.
func (r *Runner) Wait(ctx context.Context, instance, commandID, step string) (StepResult, error) {
	poll, timeout := r.Poll, r.Timeout
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	in := &ssm.GetCommandInvocationInput{CommandId: aws.String(commandID), InstanceId: aws.String(instance)}
	if step != "" {
		in.PluginName = aws.String(step)
	}
	for {
		out, err := r.SSM.GetCommandInvocation(ctx, in)
		var missing *ssmtypes.InvocationDoesNotExist
		switch {
		case errors.As(err, &missing):
			// Not delivered yet.
		case err != nil:
			return StepResult{}, fmt.Errorf("ssm: read %s step %q: %s", commandID, step, apiErrorCode(err))
		default:
			if done(out.Status) {
				return StepResult{Status: string(out.Status), ExitCode: out.ResponseCode,
					Stdout: aws.ToString(out.StandardOutputContent), Stderr: aws.ToString(out.StandardErrorContent)}, nil
			}
		}
		select {
		case <-ctx.Done():
			return StepResult{}, fmt.Errorf("ssm: command %s step %q did not finish: %w", commandID, step, ctx.Err())
		case <-time.After(poll):
		}
	}
}

func done(s ssmtypes.CommandInvocationStatus) bool {
	switch s {
	case ssmtypes.CommandInvocationStatusPending, ssmtypes.CommandInvocationStatusInProgress,
		ssmtypes.CommandInvocationStatusDelayed, ssmtypes.CommandInvocationStatusCancelling, "":
		return false
	}
	return true
}

// apiErrorCode keeps an AWS error to its code and message: the request,
// which may hold parameters, is never in it.
func apiErrorCode(err error) string {
	var ae interface {
		ErrorCode() string
		ErrorMessage() string
	}
	if errors.As(err, &ae) {
		return ae.ErrorCode() + ": " + ae.ErrorMessage()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err.Error()
	}
	return "request failed"
}

// StepError is a document step that did not succeed: its exit code (the
// scripts' header comments list them) and the script's message.
type StepError struct {
	Document string
	ExitCode int32
	Status   string
	Message  string
}

func (e *StepError) Error() string {
	return fmt.Sprintf("%s exited %d (%s): %s", e.Document, e.ExitCode, e.Status, e.Message)
}

// Exit codes of inspect.sh the platform reads as a finding, not a
// failure to retry.
const (
	InspectExitNoDocker  = 3 // docker or bash is not usable
	InspectExitImagePull = 7 // the openssl image cannot be pulled
)

// stepError describes a failed step for the onboarding's failure.
func stepError(document string, r StepResult) error {
	msg := strings.TrimSpace(r.Stderr)
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return &StepError{Document: document, ExitCode: r.ExitCode, Status: r.Status, Message: msg}
}
