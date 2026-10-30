package vpp

import (
	"context"
	"strings"
	"sync"
)

// CommandRecord is one vppctl command actually issued by the Go transaction.
// It is the Go-path equivalent of a vpp-apply step.
type CommandRecord struct {
	Operation string `json:"operation"`
	Resource  string `json:"resource,omitempty"`
	Command   string `json:"command"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ExecutionRecorder collects every operation executed through a Client. It is
// safe for concurrent use; one apply owns one recorder at a time.
type ExecutionRecorder struct {
	mu      sync.Mutex
	records []CommandRecord
}

func NewExecutionRecorder() *ExecutionRecorder {
	return &ExecutionRecorder{}
}

func (recorder *ExecutionRecorder) Reset() {
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	recorder.records = nil
	recorder.mu.Unlock()
}

func (recorder *ExecutionRecorder) Records() []CommandRecord {
	if recorder == nil {
		return nil
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]CommandRecord(nil), recorder.records...)
}

func (recorder *ExecutionRecorder) record(operation Operation, reply Reply, err error) {
	if recorder == nil {
		return
	}
	status := "applied"
	if err != nil || reply.Retval != 0 {
		status = "failed"
	} else if operationIsReadback(operation) {
		status = "readback"
	} else if strings.Contains(strings.ToLower(operation.Name), "rollback") {
		status = "rollback"
	}
	errorText := ""
	if err != nil {
		errorText = err.Error()
	}
	push := func(command, stdout, stderr string, retval int32) {
		recordStatus := status
		if retval != 0 {
			recordStatus = "failed"
		}
		recorder.mu.Lock()
		recorder.records = append(recorder.records, CommandRecord{
			Operation: operation.Name,
			Resource:  operation.Resource,
			Command:   command,
			Status:    recordStatus,
			Attempts:  1,
			Stdout:    stdout,
			Stderr:    stderr,
			Error:     errorText,
		})
		recorder.mu.Unlock()
	}
	if payload, ok := reply.Payload.(VPPCTLReplyPayload); ok && len(payload.CommandResults) > 0 {
		for _, result := range payload.CommandResults {
			command := strings.TrimSpace(result.Command)
			if command == "" {
				continue
			}
			push(command, result.Stdout, result.Stderr, result.Retval)
		}
		return
	}
	for _, command := range operation.VPPCtlCommands {
		command = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "?"))
		if command == "" || strings.HasPrefix(command, "__ly-route-vpp-batch") {
			continue
		}
		push(command, "", "", reply.Retval)
	}
	if len(operation.VPPCtlCommands) == 0 {
		push(operation.Name, "", "", reply.Retval)
	}
}

func operationIsReadback(operation Operation) bool {
	name := strings.ToLower(operation.Name)
	if strings.Contains(name, "snapshot") || strings.Contains(name, "readback") || strings.Contains(name, "verify") {
		return true
	}
	for _, command := range operation.VPPCtlCommands {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(strings.TrimPrefix(command, "?"))), "show ") {
			return true
		}
	}
	return false
}

// RecordingClient wraps a Client and records every operation/reply.
type RecordingClient struct {
	Client   Client
	Recorder *ExecutionRecorder
}

func NewRecordingClient(client Client, recorder *ExecutionRecorder) Client {
	if client == nil || recorder == nil {
		return client
	}
	return RecordingClient{Client: client, Recorder: recorder}
}

func (client RecordingClient) OpenChannel(ctx context.Context) (Channel, error) {
	channel, err := client.Client.OpenChannel(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingChannel{Channel: channel, recorder: client.Recorder}, nil
}

type recordingChannel struct {
	Channel
	recorder *ExecutionRecorder
}

func (channel *recordingChannel) Do(ctx context.Context, operation Operation) (Reply, error) {
	reply, err := channel.Channel.Do(ctx, operation)
	channel.recorder.record(operation, reply, err)
	return reply, err
}
