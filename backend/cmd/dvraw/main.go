package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func walk(b []byte, path string, depth int) {
	if depth > 6 {
		return
	}
	d := service.NewProtoDecoder(b)
	idx := 0
	for !d.Done() {
		field, wire := d.ReadTag()
		idx++
		switch wire {
		case 0:
			v := d.ReadUint64()
			fmt.Printf("%s[%d] varint=%d\n", path, field, v)
		case 1:
			d.Skip(wire)
		case 2:
			raw := d.ReadBytes()
			printable := true
			for _, c := range raw {
				if c < 0x20 || c > 0x7e {
					printable = false
					break
				}
			}
			if printable && len(raw) > 0 && len(raw) < 120 {
				fmt.Printf("%s[%d] str=%q\n", path, field, string(raw))
			} else {
				fmt.Printf("%s[%d] msg len=%d\n", path, field, len(raw))
				if len(raw) > 0 {
					walk(raw, fmt.Sprintf("%s[%d]", path, field), depth+1)
				}
			}
		case 5:
			d.Skip(wire)
		default:
			d.Skip(wire)
		}
		if idx > 200 {
			fmt.Println(path, "... truncated")
			return
		}
	}
}

func main() {
	token := os.Getenv("DEVIN_TOKEN")
	e := &service.ProtoEncoder{}
	e.Message(1, func(sub *service.ProtoEncoder) {
		sub.String(1, service.DevinClientName)
		sub.String(7, service.DevinIDEVersion)
		sub.String(12, service.DevinClientName)
		sub.String(2, service.DevinExtensionVersion)
		sub.String(3, service.NormalizeDevinToken(token))
		sub.String(4, "en")
	})
	req, _ := http.NewRequest(http.MethodPost, service.DevinAPIURL+"/exa.seat_management_pb.SeatManagementService/GetUserStatus", bytes.NewReader(e.Bytes()))
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "*/*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("err", err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	fmt.Println("status", resp.StatusCode, "bytes", len(b))
	walk(b, "root", 0)
}
