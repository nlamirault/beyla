package ebpf

import (
	"context"
	"io"
	"log/slog"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	ebpfcommon "github.com/grafana/beyla/pkg/internal/ebpf/common"
	"github.com/grafana/beyla/pkg/internal/exec"
	"github.com/grafana/beyla/pkg/internal/goexec"
	"github.com/grafana/beyla/pkg/internal/request"
	"github.com/grafana/beyla/pkg/internal/svc"
)

type PIDsAccounter interface {
	// AllowPID notifies the tracer to accept traces from the process with the
	// provided PID. Unless system-wide instrumentation, the Tracer should discard
	// traces from processes whose PID has not been allowed before
	AllowPID(uint32)
	// BlockPID notifies the tracer to stop accepting traces from the process
	// with the provided PID. After receiving them via ringbuffer, it should
	// discard them.
	BlockPID(uint32)
}

type CommonTracer interface {
	// Load the bpf object that is generated by the bpf2go compiler
	Load() (*ebpf.CollectionSpec, error)
	// AddCloser adds io.Closer instances that need to be invoked when the
	// Run function ends.
	AddCloser(c ...io.Closer)
	// BpfObjects that are created by the bpf2go compiler
	BpfObjects() any
}

type KprobesTracer interface {
	CommonTracer
	// KProbes returns a map with the name of the kernel probes that need to be
	// tapped into. Start matches kprobe, End matches kretprobe
	KProbes() map[string]ebpfcommon.FunctionPrograms
}

// Tracer is an individual eBPF program (e.g. the net/http or the grpc tracers)
type Tracer interface {
	PIDsAccounter
	KprobesTracer
	// Constants returns a map of constants to be overriden into the eBPF program.
	// The key is the constant name and the value is the value to overwrite.
	Constants(*exec.FileInfo, *goexec.Offsets) map[string]any
	// GoProbes returns a map with the name of Go functions that need to be inspected
	// in the executable, as well as the eBPF programs that optionally need to be
	// inserted as the Go function start and end probes
	GoProbes() map[string]ebpfcommon.FunctionPrograms
	// UProbes returns a map with the module name mapping to the uprobes that need to be
	// tapped into. Start matches uprobe, End matches uretprobe
	UProbes() map[string]map[string]ebpfcommon.FunctionPrograms
	// SocketFilters  returns a list of programs that need to be loaded as a
	// generic eBPF socket filter
	SocketFilters() []*ebpf.Program
	// Run will do the action of listening for eBPF traces and forward them
	// periodically to the output channel.
	// It optionally receives the service svc.ID, to
	// populate each forwarded span with its value. But some
	// tracers might ignore it (e.g. system-wide HTTP filter will directly set the
	// executable name of each request).
	Run(context.Context, chan<- []request.Span, svc.ID)
}

// Subset of the above interface, which supports loading eBPF programs which
// are not tied to service monitoring
type UtilityTracer interface {
	KprobesTracer
	Run(context.Context)
}

// ProcessTracer instruments an executable with eBPF and provides the eBPF readers
// that will forward the traces to later stages in the pipeline
type ProcessTracer struct {
	log      *slog.Logger //nolint:unused
	Programs []Tracer
	ELFInfo  *exec.FileInfo
	Goffsets *goexec.Offsets
	Exe      *link.Executable
	PinPath  string

	SystemWide bool
}

func (pt *ProcessTracer) AllowPID(pid uint32) {
	for i := range pt.Programs {
		pt.Programs[i].AllowPID(pid)
	}
}

func (pt *ProcessTracer) BlockPID(pid uint32) {
	for i := range pt.Programs {
		pt.Programs[i].BlockPID(pid)
	}
}
