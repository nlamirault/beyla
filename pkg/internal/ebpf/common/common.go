package ebpfcommon

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"

	"github.com/grafana/beyla/pkg/internal/goexec"
	"github.com/grafana/beyla/pkg/internal/request"
)

//go:generate $BPF2GO -cc $BPF_CLANG -cflags $BPF_CFLAGS -target amd64,arm64 -type http_request_trace -type sql_request_trace -type http_info_t -type connection_info_t -type http2_grpc_request_t -type tcp_req_t bpf ../../../../bpf/http_trace.c -- -I../../../../bpf/headers

// HTTPRequestTrace contains information from an HTTP request as directly received from the
// eBPF layer. This contains low-level C structures for accurate binary read from ring buffer.
type HTTPRequestTrace bpfHttpRequestTrace
type SQLRequestTrace bpfSqlRequestTrace
type BPFHTTPInfo bpfHttpInfoT
type BPFConnInfo bpfConnectionInfoT

const EventTypeSQL = 5    // EVENT_SQL_CLIENT
const EventTypeKHTTP = 6  // HTTP Events generated by kprobes
const EventTypeKHTTP2 = 7 // HTTP2/gRPC Events generated by kprobes
const EventTypeTCP = 8    // Unknown TCP protocol to be classified by user space

var IntegrityModeOverride = false

var ActiveNamespaces = make(map[uint32]uint32)

// TracerConfig configuration for eBPF programs
type TracerConfig struct {
	BpfDebug bool `yaml:"bfp_debug" env:"BEYLA_BPF_DEBUG"`

	// WakeupLen specifies how many messages need to be accumulated in the eBPF ringbuffer
	// before sending a wakeup request.
	// High values of WakeupLen could add a noticeable metric delay in services with low
	// requests/second.
	// TODO: see if there is a way to force eBPF to wakeup userspace on timeout
	WakeupLen int `yaml:"wakeup_len" env:"BEYLA_BPF_WAKEUP_LEN"`
	// BatchLength allows specifying how many traces will be batched at the initial
	// stage before being forwarded to the next stage
	BatchLength int `yaml:"batch_length" env:"BEYLA_BPF_BATCH_LENGTH"`
	// BatchTimeout specifies the timeout to forward the data batch if it didn't
	// reach the BatchLength size
	BatchTimeout time.Duration `yaml:"batch_timeout" env:"BEYLA_BPF_BATCH_TIMEOUT"`

	// BpfBaseDir specifies the base directory where the BPF pinned maps will be mounted.
	// By default, it will be /var/run/beyla
	BpfBaseDir string `yaml:"bpf_fs_base_dir" env:"BEYLA_BPF_FS_BASE_DIR"`

	// BpfPath specifies the path in the base directory where the BPF pinned maps will be mounted.
	// By default, it will be beyla-<pid>.
	BpfPath string `yaml:"bpf_fs_path" env:"BEYLA_BPF_FS_PATH"`

	// If enabled, the kprobes based HTTP request tracking will start tracking the request
	// headers to process any 'Traceparent' fields.
	TrackRequestHeaders bool `yaml:"track_request_headers" env:"BEYLA_BPF_TRACK_REQUEST_HEADERS"`

	HTTPRequestTimeout time.Duration `yaml:"http_request_timeout" env:"BEYLA_BPF_HTTP_REQUEST_TIMEOUT"`
}

// Probe holds the information of the instrumentation points of a given function: its start and end offsets and
// eBPF programs
type Probe struct {
	Offsets  goexec.FuncOffsets
	Programs FunctionPrograms
}

type FunctionPrograms struct {
	// Required, if true, will cancel the execution of the eBPF Tracer
	// if the function has not been found in the executable
	Required bool
	Start    *ebpf.Program
	End      *ebpf.Program
}

type Filter struct {
	io.Closer
	Fd int
}

func ptlog() *slog.Logger { return slog.With("component", "ebpf.ProcessTracer") }

func ReadHTTPRequestTraceAsSpan(record *ringbuf.Record) (request.Span, bool, error) {
	var eventType uint8

	// we read the type first, depending on the type we decide what kind of record we have
	err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &eventType)
	if err != nil {
		return request.Span{}, true, err
	}

	switch eventType {
	case EventTypeSQL:
		return ReadSQLRequestTraceAsSpan(record)
	case EventTypeKHTTP:
		return ReadHTTPInfoIntoSpan(record)
	case EventTypeKHTTP2:
		return ReadHTTP2InfoIntoSpan(record)
	case EventTypeTCP:
		return ReadTCPRequestIntoSpan(record)
	}

	var event HTTPRequestTrace

	err = binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event)
	if err != nil {
		return request.Span{}, true, err
	}

	return HTTPRequestTraceToSpan(&event), false, nil
}

func ReadSQLRequestTraceAsSpan(record *ringbuf.Record) (request.Span, bool, error) {
	var event SQLRequestTrace
	if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
		return request.Span{}, true, err
	}

	return SQLRequestTraceToSpan(&event), false, nil
}

type KernelLockdown uint8

const (
	KernelLockdownNone KernelLockdown = iota + 1
	KernelLockdownIntegrity
	KernelLockdownConfidentiality
	KernelLockdownOther
)

func SupportsContextPropagation(log *slog.Logger) bool {
	kernelMajor, kernelMinor := KernelVersion()
	log.Debug("Linux kernel version", "major", kernelMajor, "minor", kernelMinor)

	if kernelMajor < 5 || (kernelMajor == 5 && kernelMinor < 10) {
		log.Debug("Found Linux kernel earlier than 5.10, trace context propagation is supported", "major", kernelMajor, "minor", kernelMinor)
		return true
	}

	lockdown := KernelLockdownMode()

	if lockdown == KernelLockdownNone {
		log.Debug("Kernel not in lockdown mode, trace context propagation is supported.")
		return true
	}

	return false
}

func SupportsEBPFLoops() bool {
	kernelMajor, kernelMinor := KernelVersion()
	return kernelMajor > 5 || (kernelMajor == 5 && kernelMinor >= 17)
}

// Injectable for tests
var lockdownPath = "/sys/kernel/security/lockdown"

func KernelLockdownMode() KernelLockdown {
	plog := ptlog()
	plog.Debug("checking kernel lockdown mode, [none] allows us to propagate trace context")
	// If we can't find the file, assume no lockdown
	if _, err := os.Stat(lockdownPath); err == nil {
		f, err := os.Open(lockdownPath)

		if err != nil {
			plog.Warn("failed to open /sys/kernel/security/lockdown, assuming lockdown [integrity]", "error", err)
			return KernelLockdownIntegrity
		}

		defer f.Close()
		scanner := bufio.NewScanner(f)
		if scanner.Scan() {
			lockdown := scanner.Text()
			switch {
			case strings.Contains(lockdown, "[none]"):
				return KernelLockdownNone
			case strings.Contains(lockdown, "[integrity]"):
				return KernelLockdownIntegrity
			case strings.Contains(lockdown, "[confidentiality]"):
				return KernelLockdownConfidentiality
			default:
				return KernelLockdownOther
			}
		}

		plog.Warn("file /sys/kernel/security/lockdown is empty, assuming lockdown [integrity]")
		return KernelLockdownIntegrity
	}

	plog.Debug("can't find /sys/kernel/security/lockdown, assuming no lockdown")
	return KernelLockdownNone
}

func cstr(chars []uint8) string {
	addrLen := bytes.IndexByte(chars, 0)
	if addrLen < 0 {
		addrLen = len(chars)
	}

	return string(chars[:addrLen])
}
