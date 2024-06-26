package ebpfcommon

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"

	"github.com/cilium/ebpf/ringbuf"
	trace2 "go.opentelemetry.io/otel/trace"

	"github.com/grafana/beyla/pkg/internal/request"
	"github.com/grafana/beyla/pkg/internal/sqlprune"
)

type TCPRequestInfo bpfTcpReqT

func ReadTCPRequestIntoSpan(record *ringbuf.Record) (request.Span, bool, error) {
	var event TCPRequestInfo

	err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event)
	if err != nil {
		return request.Span{}, true, err
	}

	b := event.Buf[:]

	l := int(event.Len)
	if l < 0 || len(b) < l {
		l = len(b)
	}

	buf := string(event.Buf[:l])

	// Check if we have a SQL statement
	sqlIndex := isSQL(buf)
	if sqlIndex >= 0 {
		return TCPToSQLToSpan(&event, buf[sqlIndex:]), false, nil
	}

	return request.Span{}, true, nil // ignore if we couldn't parse it
}

func isSQL(buf string) int {
	b := strings.ToUpper(buf)
	for _, q := range []string{"SELECT", "UPDATE", "DELETE", "INSERT", "ALTER", "CREATE", "DROP"} {
		i := strings.Index(b, q)
		if i >= 0 {
			return i
		}
	}

	return -1
}

func (trace *TCPRequestInfo) reqHostInfo() (source, target string) {
	src := make(net.IP, net.IPv6len)
	dst := make(net.IP, net.IPv6len)
	copy(src, trace.ConnInfo.S_addr[:])
	copy(dst, trace.ConnInfo.D_addr[:])

	return src.String(), dst.String()
}

func TCPToSQLToSpan(trace *TCPRequestInfo, s string) request.Span {
	sql := cstr([]uint8(s))

	method, path := sqlprune.SQLParseOperationAndTable(sql)

	peer := ""
	hostname := ""
	hostPort := 0

	if trace.ConnInfo.S_port != 0 || trace.ConnInfo.D_port != 0 {
		peer, hostname = trace.reqHostInfo()
		hostPort = int(trace.ConnInfo.D_port)
	}

	return request.Span{
		Type:          request.EventTypeSQLClient,
		Method:        method,
		Path:          path,
		Peer:          peer,
		Host:          hostname,
		HostPort:      hostPort,
		ContentLength: 0,
		RequestStart:  int64(trace.StartMonotimeNs),
		Start:         int64(trace.StartMonotimeNs),
		End:           int64(trace.EndMonotimeNs),
		Status:        0,
		TraceID:       trace2.TraceID(trace.Tp.TraceId),
		SpanID:        trace2.SpanID(trace.Tp.SpanId),
		ParentSpanID:  trace2.SpanID(trace.Tp.ParentId),
		Flags:         trace.Tp.Flags,
		Pid: request.PidInfo{
			HostPID:   trace.Pid.HostPid,
			UserPID:   trace.Pid.UserPid,
			Namespace: trace.Pid.Ns,
		},
		Statement: sql,
	}
}
