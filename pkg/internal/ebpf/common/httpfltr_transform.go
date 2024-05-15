package ebpfcommon

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cilium/ebpf/ringbuf"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/beyla/pkg/internal/request"
	"github.com/grafana/beyla/pkg/internal/svc"
)

func httpInfoToSpan(info *HTTPInfo) request.Span {
	return request.Span{
		Type:          request.EventType(info.Type),
		ID:            0,
		Method:        info.Method,
		Path:          removeQuery(info.URL),
		Peer:          info.Peer,
		Host:          info.Host,
		HostPort:      int(info.ConnInfo.D_port),
		ContentLength: int64(info.Len),
		RequestStart:  int64(info.StartMonotimeNs),
		Start:         int64(info.StartMonotimeNs),
		End:           int64(info.EndMonotimeNs),
		Status:        int(info.Status),
		ServiceID:     info.Service,
		TraceID:       trace.TraceID(info.Tp.TraceId),
		SpanID:        trace.SpanID(info.Tp.SpanId),
		ParentSpanID:  trace.SpanID(info.Tp.ParentId),
		Flags:         info.Tp.Flags,
		Pid: request.PidInfo{
			HostPID:   info.Pid.HostPid,
			UserPID:   info.Pid.UserPid,
			Namespace: info.Pid.Ns,
		},
	}
}

func removeQuery(url string) string {
	idx := strings.IndexByte(url, '?')
	if idx > 0 {
		return url[:idx]
	}
	return url
}

type HTTPInfo struct {
	BPFHTTPInfo
	Method  string
	URL     string
	Host    string
	Peer    string
	Service svc.ID
}

func ReadHTTPInfoIntoSpan(record *ringbuf.Record) (request.Span, bool, error) {
	var event BPFHTTPInfo
	var result HTTPInfo

	err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event)
	if err != nil {
		return request.Span{}, true, err
	}

	result = HTTPInfo{BPFHTTPInfo: event}

	// When we can't find the connection info, we signal that through making the
	// source and destination ports equal to max short. E.g. async SSL
	if event.ConnInfo.S_port != 0 || event.ConnInfo.D_port != 0 {
		source, target := event.hostInfo()
		result.Host = target
		result.Peer = source
	} else {
		host, port := event.hostFromBuf()

		if port >= 0 {
			result.Host = host
			result.ConnInfo.D_port = uint16(port)
		}
	}
	result.URL = event.url()
	result.Method = event.method()
	// set generic service to be overwritten later by the PID filters
	result.Service = svc.ID{SDKLanguage: svc.InstrumentableGeneric}

	return httpInfoToSpan(&result), false, nil
}

func (event *BPFHTTPInfo) url() string {
	buf := string(event.Buf[:])
	space := strings.Index(buf, " ")
	if space < 0 {
		return ""
	}

	bufEnd := bytes.IndexByte(event.Buf[:], 0) // We assume the buffer was zero initialized in eBPF
	if bufEnd < 0 {
		bufEnd = len(buf)
	}

	if space+1 > bufEnd {
		return ""
	}

	nextSpace := strings.IndexAny(buf[space+1:bufEnd], " \r\n")
	if nextSpace < 0 {
		return buf[space+1 : bufEnd]
	}

	end := nextSpace + space + 1
	if end > bufEnd {
		end = bufEnd
	}

	return buf[space+1 : end]
}

func (event *BPFHTTPInfo) method() string {
	buf := string(event.Buf[:])
	space := strings.Index(buf, " ")
	if space < 0 {
		return ""
	}

	return buf[:space]
}

func (event *BPFHTTPInfo) hostFromBuf() (string, int) {
	buf := cstr(event.Buf[:])

	host := "Host: "
	idx := strings.Index(buf, host)

	if idx < 0 {
		return "", -1
	}

	buf = buf[idx+len(host):]

	rIdx := strings.Index(buf, "\r")

	if rIdx < 0 {
		rIdx = len(buf)
	}

	host, portStr, err := net.SplitHostPort(buf[:rIdx])

	if err != nil {
		return "", -1
	}

	port, _ := strconv.Atoi(portStr)

	return host, port
}

func (event *BPFHTTPInfo) hostInfo() (source, target string) {
	src := make(net.IP, net.IPv6len)
	dst := make(net.IP, net.IPv6len)
	copy(src, event.ConnInfo.S_addr[:])
	copy(dst, event.ConnInfo.D_addr[:])

	return src.String(), dst.String()
}

func commName(pid uint32) string {
	procPath := filepath.Join("/proc", strconv.FormatUint(uint64(pid), 10), "comm")
	_, err := os.Stat(procPath)
	if os.IsNotExist(err) {
		return ""
	}

	name, err := os.ReadFile(procPath)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(name))
}
