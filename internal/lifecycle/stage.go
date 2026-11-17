package lifecycle

import "strings"

// unknownName is what a value outside the defined ones is called.
const unknownName = "UNKNOWN"

// Stage is a point in the process lifecycle at which init hooks run.
type Stage int

// The stages, in the order they happen.
const (
	// StageBoot runs before the gateway starts.
	StageBoot Stage = iota + 1
	// StageStart runs while the services start.
	StageStart
	// StageReady runs once the gateway serves.
	StageReady
	// StageShutdown runs when the process stops.
	StageShutdown
)

// Stages lists every stage in order.
func Stages() []Stage {
	return []Stage{StageBoot, StageStart, StageReady, StageShutdown}
}

// String returns the stage's name as LocalStack reports it: BOOT, START, READY or
// SHUTDOWN.
func (s Stage) String() string {
	switch s {
	case StageBoot:
		return "BOOT"
	case StageStart:
		return "START"
	case StageReady:
		return "READY"
	case StageShutdown:
		return "SHUTDOWN"
	default:
		return unknownName
	}
}

// directory returns the hook directory of the stage, for example "boot.d".
func (s Stage) directory() string {
	return strings.ToLower(s.String()) + ".d"
}

// ParseStage finds a stage by name, ignoring case.
func ParseStage(name string) (Stage, bool) {
	for _, stage := range Stages() {
		if strings.EqualFold(stage.String(), name) {
			return stage, true
		}
	}

	return 0, false
}

// State is where a hook script is in its run.
type State int

// The script states, with LocalStack's names.
const (
	// StateUnknown is a script that has not run.
	StateUnknown State = iota
	// StateRunning is a script that is running.
	StateRunning
	// StateSuccessful is a script that exited with status 0.
	StateSuccessful
	// StateError is a script that failed to start or exited with another status.
	StateError
)

// String returns the state's name as LocalStack reports it.
func (s State) String() string {
	switch s {
	case StateRunning:
		return "RUNNING"
	case StateSuccessful:
		return "SUCCESSFUL"
	case StateError:
		return "ERROR"
	default:
		return unknownName
	}
}

// Script is one init hook.
type Script struct {
	// Path is the absolute path of the script.
	Path string
	// Stage is the stage the script belongs to.
	Stage Stage
	// State is the outcome of its last run.
	State State
}
