package main

import (
	"fmt"

	"github.com/jonbaldie/beads/internal/storage/dbproxy/proxy"
)

// doltStopResult is the shared JSON object for successful and refused stop
// operations. Force-stop recovery deliberately exposes each irreversible
// action so automation can distinguish a matched executable from a signaled
// process and a quarantined record from one left in place.
type doltStopProcessState struct {
	RecordLeftAlone  bool   `json:"record_left_alone,omitempty"`
	ProcessWasGone   bool   `json:"process_was_gone,omitempty"`
	SignalSent       bool   `json:"signal_sent,omitempty"`
	ProcessLeftAlone bool   `json:"process_left_alone,omitempty"`
	QuarantinedPath  string `json:"quarantined_path,omitempty"`
}

type doltStopResult struct {
	Stopped               bool                  `json:"stopped"`
	Force                 bool                  `json:"force"`
	ForcedRecovery        bool                  `json:"forced_recovery,omitempty"`
	Verified              *bool                 `json:"verified,omitempty"`
	VerifiedShutdownError string                `json:"verified_shutdown_error,omitempty"`
	RecordFound           bool                  `json:"record_found,omitempty"`
	RecordPath            string                `json:"record_path,omitempty"`
	LockWasHeld           bool                  `json:"lock_was_held,omitempty"`
	PID                   int                   `json:"pid,omitempty"`
	Executable            string                `json:"executable,omitempty"`
	ExecutableVerified    *bool                 `json:"executable_verified,omitempty"`
	Backend               *doltStopRecordResult `json:"backend,omitempty"`
	Error                 string                `json:"error,omitempty"`
	doltStopProcessState
}

// doltStopRecordResult mirrors the per-record force-stop fields for the
// backend (proxy-child) record.
type doltStopRecordResult struct {
	RecordFound     bool   `json:"record_found,omitempty"`
	RecordPath      string `json:"record_path,omitempty"`
	LockWasHeld     bool   `json:"lock_was_held,omitempty"`
	PID             int    `json:"pid,omitempty"`
	Executable      string `json:"executable,omitempty"`
	ProcessWasGone  bool   `json:"process_was_gone,omitempty"`
	SignalSent      bool   `json:"signal_sent,omitempty"`
	QuarantinedPath string `json:"quarantined_path,omitempty"`
}

func newForcedDoltStopResult(
	shutdownErr error,
	report proxy.ForceStopReport,
	forceErr error,
) doltStopResult {
	result := doltStopResult{
		Stopped:               forceErr == nil,
		Force:                 true,
		ForcedRecovery:        true,
		Verified:              boolPointer(false),
		VerifiedShutdownError: shutdownErr.Error(),
		RecordFound:           report.RecordFound,
		RecordPath:            report.RecordPath,
		LockWasHeld:           report.LockWasHeld,
		PID:                   report.PID,
		Executable:            report.Executable,
		doltStopProcessState: doltStopProcessState{
			ProcessWasGone:  report.ProcessWasGone,
			SignalSent:      report.SignalSent,
			QuarantinedPath: report.QuarantinedPath,
		},
	}
	if report.Executable != "" {
		result.ExecutableVerified = boolPointer(
			report.Executable == "bd" || report.Executable == "dolt",
		)
	}
	result.ProcessLeftAlone = report.RecordFound &&
		!report.ProcessWasGone &&
		!report.SignalSent
	result.RecordLeftAlone = report.RecordFound && report.QuarantinedPath == ""
	if report.Backend != nil {
		result.Backend = &doltStopRecordResult{
			RecordFound:     report.Backend.RecordFound,
			RecordPath:      report.Backend.RecordPath,
			LockWasHeld:     report.Backend.LockWasHeld,
			PID:             report.Backend.PID,
			Executable:      report.Backend.Executable,
			ProcessWasGone:  report.Backend.ProcessWasGone,
			SignalSent:      report.Backend.SignalSent,
			QuarantinedPath: report.Backend.QuarantinedPath,
		}
	}
	if forceErr != nil {
		result.Error = forceErr.Error()
	}
	return result
}

func boolPointer(value bool) *bool {
	return &value
}

func renderDoltStopResult(result doltStopResult) error {
	if isJSONOutput() {
		return renderDoltStopJSON(result)
	}
	if !result.ForcedRecovery {
		fmt.Println("Dolt server stopped.")
		return nil
	}
	printDoltStopRecovery(result)
	if result.Error != "" {
		return HandleError("%s", result.Error)
	}
	return nil
}

func renderDoltStopJSON(result doltStopResult) error {
	if err := outputJSON(result); err != nil {
		return HandleError("encode dolt stop result: %v", err)
	}
	if result.Error != "" {
		return SilentExit()
	}
	return nil
}

func printDoltStopRecovery(result doltStopResult) {
	fmt.Printf("Verified shutdown refused: %s\n", result.VerifiedShutdownError)
	if result.Error == "" {
		fmt.Println("Dolt server stopped with --force.")
	} else if result.ProcessLeftAlone {
		fmt.Println("Force stop refused; the recorded process was left alone.")
	} else {
		fmt.Println("Force stop incomplete; completed actions are reported below.")
	}
	if result.RecordFound {
		fmt.Printf("  Record: %s\n", result.RecordPath)
	}
	if result.PID != 0 {
		fmt.Printf("  PID: %d\n", result.PID)
	}
	if result.Executable != "" {
		printDoltStopExecutable(result)
	}
	printDoltStopProcess(result)
	printDoltStopRecord(result)
	printDoltStopBackend(result.Backend)
}

func printDoltStopExecutable(result doltStopResult) {
	if result.ExecutableVerified != nil && *result.ExecutableVerified {
		fmt.Printf("  Executable: %s (matched bd/dolt)\n", result.Executable)
		return
	}
	fmt.Printf("  Executable: %s (not bd/dolt)\n", result.Executable)
}

func printDoltStopProcess(result doltStopResult) {
	switch {
	case result.SignalSent:
		fmt.Println("  Process: signal sent")
	case result.ProcessWasGone:
		fmt.Println("  Process: already gone; no signal sent")
	case result.ProcessLeftAlone:
		fmt.Println("  Process: left alone; no signal sent")
	}
}

func printDoltStopRecord(result doltStopResult) {
	switch {
	case result.QuarantinedPath != "":
		fmt.Printf("  Record quarantined: %s\n", result.QuarantinedPath)
	case result.RecordLeftAlone:
		fmt.Println("  Record: left unchanged")
	}
}

func printDoltStopBackend(backend *doltStopRecordResult) {
	if backend == nil {
		return
	}
	fmt.Printf("  Backend record: %s\n", backend.RecordPath)
	if backend.PID != 0 {
		fmt.Printf("  Backend PID: %d\n", backend.PID)
	}
	switch {
	case backend.SignalSent:
		fmt.Println("  Backend process: signal sent")
	case backend.ProcessWasGone:
		fmt.Println("  Backend process: already gone; no signal sent")
	default:
		fmt.Println("  Backend process: left alone; no signal sent")
	}
	if backend.QuarantinedPath != "" {
		fmt.Printf("  Backend record quarantined: %s\n", backend.QuarantinedPath)
	} else {
		fmt.Println("  Backend record: left unchanged")
	}
}
