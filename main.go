// wetype-x11 — 单文件微信输入法(WeType)流式语音转文字工具(X11)
//
// 按一次开始录音,再按一次结束并把识别结果用 xdotool 输入到当前窗口。
//
// sxhkd 绑定示例:
//     ctrl + alt + z  →  /path/to/wetype-x11
//     ctrl + alt + x  →  /path/to/wetype-x11 --cancel
//
// 运行期依赖: pw-record|parec|arecord(任一) + xdotool
// 构建: 见 build.sh (静态链接 libopus, 产物零动态依赖)
//
// WeType 协议移植自 wetype-x11.py (源自 xifan2333/vinput-registry
// wetype/streaming/entry.py; 协议参考 J3n5en/voicekey, 客户端参数 2.2.3(657))。
// 录音会发送到微信输入法云端识别服务。
package main

/*
#cgo CFLAGS: -I${SRCDIR}/vendor/opus/include
#cgo LDFLAGS: ${SRCDIR}/vendor/opus/lib/libopus.a -lm
#include <opus/opus.h>

static OpusEncoder* opus_enc_create(int rate, int ch, int app, int *err) {
	return opus_encoder_create(rate, ch, app, err);
}
static int opus_enc_set_bitrate(OpusEncoder *st, int v) {
	return opus_encoder_ctl(st, OPUS_SET_BITRATE(v));
}
static int opus_enc_set_complexity(OpusEncoder *st, int v) {
	return opus_encoder_ctl(st, OPUS_SET_COMPLEXITY(v));
}
static int opus_enc_encode(OpusEncoder *st, const opus_int16 *pcm, int frame_size,
                           unsigned char *out, int max) {
	return opus_encode(st, pcm, frame_size, out, max);
}
static void opus_enc_destroy(OpusEncoder *st) { opus_encoder_destroy(st); }
*/
import "C"

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ---------------------------------------------------------------- 配置

var (
	runtimeDir   = os.Getenv("XDG_RUNTIME_DIR")
	cacheDir     = filepath.Join(xdgOr("XDG_CACHE_HOME", filepath.Join(home(), ".cache")), "wetype-x11")
	pidFile      string
	logFile      string
	credPath     string
	maxSecs      = envFloat("WETYPE_X11_MAX_SECS", 300)
	ioTimeout    = envFloat("VINPUT_ASR_TIMEOUT", 15)
	finishGrace  = envFloat("VINPUT_ASR_FINISH_GRACE_SECS", 5)
)

func init() {
	if runtimeDir == "" {
		runtimeDir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	pidFile = filepath.Join(runtimeDir, "wetype-x11.pid")
	logFile = filepath.Join(cacheDir, "wetype-x11.log")
	credPath = filepath.Join(cacheDir, "identity.json")
}

func home() string { h, _ := os.UserHomeDir(); return h }

func xdgOr(envName, def string) string {
	if v := os.Getenv(envName); v != "" {
		return v
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v > 0 {
		return v
	}
	return def
}

// ---------------------------------------------------------------- Opus

type OpusEncoder struct{ enc *C.OpusEncoder }

func NewOpusEncoder() (*OpusEncoder, error) {
	var cerr C.int
	enc := C.opus_enc_create(16000, 1, 2049, &cerr) // OPUS_APPLICATION_AUDIO
	if enc == nil || cerr != 0 {
		return nil, fmt.Errorf("opus_encoder_create failed: %d", int(cerr))
	}
	if r := C.opus_enc_set_bitrate(enc, 64000); r != 0 {
		return nil, fmt.Errorf("opus set bitrate failed: %d", int(r))
	}
	if r := C.opus_enc_set_complexity(enc, 9); r != 0 {
		return nil, fmt.Errorf("opus set complexity failed: %d", int(r))
	}
	return &OpusEncoder{enc: enc}, nil
}

func (e *OpusEncoder) Encode(pcm []byte) ([]byte, error) {
	if len(pcm) != 640 {
		return nil, fmt.Errorf("opus frame must be 640 bytes (320 samples), got %d", len(pcm))
	}
	out := make([]byte, 4000)
	n := C.opus_enc_encode(e.enc, (*C.opus_int16)(unsafe.Pointer(&pcm[0])), 320,
		(*C.uchar)(unsafe.Pointer(&out[0])), 4000)
	if n < 0 {
		return nil, fmt.Errorf("opus_encode failed: %d", int(n))
	}
	return out[:n], nil
}

func (e *OpusEncoder) Close() { C.opus_enc_destroy(e.enc) }

// ---------------------------------------------------------------- WebSocket

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsClient struct {
	conn    net.Conn
	buf     []byte
	closed  bool
	timeout time.Duration
}

func newWSClient(host string, headers map[string]string, timeout float64) (*wsClient, error) {
	to := time.Duration(timeout * float64(time.Second))
	raw, err := net.DialTimeout("tcp", host+":443", to)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(raw, &tls.Config{ServerName: host})
	tlsConn.SetDeadline(time.Now().Add(to))
	if err := tlsConn.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	keyBytes := make([]byte, 16)
	rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	var sb strings.Builder
	sb.WriteString("GET / HTTP/1.1\r\nHost: " + host + "\r\n")
	sb.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\n")
	sb.WriteString("Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n")
	for k, v := range headers {
		sb.WriteString(k + ": " + v + "\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := tlsConn.Write([]byte(sb.String())); err != nil {
		tlsConn.Close()
		return nil, err
	}
	c := &wsClient{conn: tlsConn, timeout: to}
	resp, err := c.readHTTPResponse()
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	headerBlob := strings.SplitN(resp, "\r\n\r\n", 2)[0]
	lines := strings.Split(headerBlob, "\r\n")
	parts := strings.Fields(lines[0])
	if len(parts) < 2 || parts[1] != "101" {
		tlsConn.Close()
		return nil, fmt.Errorf("websocket handshake failed: %s", lines[0])
	}
	respHeaders := map[string]string{}
	for _, l := range lines[1:] {
		if i := strings.Index(l, ":"); i >= 0 {
			respHeaders[strings.ToLower(strings.TrimSpace(l[:i]))] = strings.TrimSpace(l[i+1:])
		}
	}
	h := sha1.Sum([]byte(key + wsGUID))
	if respHeaders["sec-websocket-accept"] != base64.StdEncoding.EncodeToString(h[:]) {
		tlsConn.Close()
		return nil, errors.New("websocket handshake failed: bad Sec-WebSocket-Accept")
	}
	tlsConn.SetDeadline(time.Time{})
	return c, nil
}

func (c *wsClient) readHTTPResponse() (string, error) {
	data := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for !bytes.Contains(data, []byte("\r\n\r\n")) {
		n, err := c.conn.Read(tmp)
		if n > 0 {
			data = append(data, tmp[:n]...)
		}
		if err != nil {
			return "", err
		}
		if len(data) > 65536 {
			return "", errors.New("websocket handshake: response too large")
		}
	}
	parts := bytes.SplitN(data, []byte("\r\n\r\n"), 2)
	c.buf = append(c.buf, parts[1]...)
	return string(data), nil
}

func (c *wsClient) sendFrame(opcode byte, payload []byte) error {
	if c.closed {
		return nil
	}
	header := []byte{0x80 | opcode}
	mask := make([]byte, 4)
	rand.Read(mask)
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n < 65536:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		header = append(header, b[:]...)
	}
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
	_, err := c.conn.Write(append(append(header, mask...), masked...))
	return err
}

func (c *wsClient) recvExact(n int) ([]byte, error) {
	for len(c.buf) < n {
		c.conn.SetReadDeadline(time.Now().Add(c.timeout))
		chunk := make([]byte, 4096)
		m, err := c.conn.Read(chunk)
		if m > 0 {
			c.buf = append(c.buf, chunk[:m]...)
		}
		if err != nil {
			return nil, err
		}
	}
	out := c.buf[:n]
	c.buf = c.buf[n:]
	return out, nil
}

func (c *wsClient) recvFrame() (opcode byte, payload []byte, fin bool, err error) {
	h, err := c.recvExact(2)
	if err != nil {
		return 0, nil, false, err
	}
	fin = h[0]&0x80 != 0
	opcode = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	length := uint64(h[1] & 0x7F)
	if length == 126 {
		b, err := c.recvExact(2)
		if err != nil {
			return 0, nil, false, err
		}
		length = uint64(binary.BigEndian.Uint16(b))
	} else if length == 127 {
		b, err := c.recvExact(8)
		if err != nil {
			return 0, nil, false, err
		}
		length = binary.BigEndian.Uint64(b)
	}
	if length > 8*1024*1024 {
		return 0, nil, false, errors.New("websocket frame too large")
	}
	var mask []byte
	if masked {
		if mask, err = c.recvExact(4); err != nil {
			return 0, nil, false, err
		}
	}
	payload, err = c.recvExact(int(length))
	if err != nil {
		return 0, nil, false, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, fin, nil
}

func (c *wsClient) sendBinary(payload []byte) error { return c.sendFrame(0x2, payload) }

func (c *wsClient) recvBinary() ([]byte, error) {
	var fragments []byte
	var curOpcode byte = 0xFF
	for {
		opcode, payload, fin, err := c.recvFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 0x8:
			c.closed = true
			return nil, errors.New("websocket closed by peer")
		case 0x9:
			c.sendFrame(0xA, payload)
			continue
		case 0xA:
			continue
		}
		if opcode != 0x0 && opcode != 0x2 {
			continue
		}
		if opcode == 0x2 {
			curOpcode = opcode
			fragments = payload
		} else {
			if curOpcode == 0xFF {
				continue
			}
			fragments = append(fragments, payload...)
		}
		if len(fragments) > 8*1024*1024 {
			return nil, errors.New("websocket message too large")
		}
		if !fin {
			continue
		}
		return fragments, nil
	}
}

func (c *wsClient) abort() {
	c.closed = true
	c.conn.Close()
}

// ---------------------------------------------------------------- AES-256-ECB (PKCS#7)

func aesCrypt(key, data []byte, encrypt bool) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("AES key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if encrypt {
		pad := 16 - len(data)%16
		data = append(append([]byte{}, data...), bytes.Repeat([]byte{byte(pad)}, pad)...)
		out := make([]byte, len(data))
		for i := 0; i < len(data); i += 16 {
			block.Encrypt(out[i:], data[i:])
		}
		return out, nil
	}
	if len(data)%16 != 0 || len(data) == 0 {
		return nil, errors.New("AES ciphertext length invalid")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += 16 {
		block.Decrypt(out[i:], data[i:])
	}
	pad := int(out[len(out)-1])
	if pad < 1 || pad > 16 || !bytes.Equal(out[len(out)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("AES padding invalid")
	}
	return out[:len(out)-pad], nil
}

// ---------------------------------------------------------------- protobuf wire

func varint(n uint64) []byte {
	var out []byte
	for n > 127 {
		out = append(out, byte(n&127)|128)
		n >>= 7
	}
	return append(out, byte(n))
}

func readVarint(data []byte, i int) (uint64, int, error) {
	var n uint64
	for shift := 0; shift < 70; shift += 7 {
		if i >= len(data) {
			return 0, 0, errors.New("truncated varint")
		}
		b := data[i]
		i++
		n |= uint64(b&127) << shift
		if b < 128 {
			return n, i, nil
		}
	}
	return 0, 0, errors.New("invalid varint")
}

// field 编码: v 支持 string / []byte / int / uint64
func field(n int, v interface{}) []byte {
	switch t := v.(type) {
	case string:
		v = []byte(t)
	case int:
		v = uint64(t)
	}
	switch t := v.(type) {
	case []byte:
		out := varint(uint64(n*8 + 2))
		out = append(out, varint(uint64(len(t)))...)
		return append(out, t...)
	case uint64:
		return append(varint(uint64(n*8)), varint(t)...)
	}
	panic("unsupported field type")
}

type pfield struct {
	num     int
	isBytes bool
	v       uint64
	b       []byte
}

func parseFields(data []byte) ([]pfield, error) {
	var out []pfield
	i := 0
	for i < len(data) {
		tag, i2, err := readVarint(data, i)
		if err != nil {
			return nil, err
		}
		i = i2
		wire := tag & 7
		switch wire {
		case 0:
			v, i3, err := readVarint(data, i)
			if err != nil {
				return nil, err
			}
			i = i3
			out = append(out, pfield{int(tag >> 3), false, v, nil})
		case 1, 2, 5:
			var size uint64
			if wire == 2 {
				s, i3, err := readVarint(data, i)
				if err != nil {
					return nil, err
				}
				size, i = s, i3
			} else if wire == 1 {
				size = 8
			} else {
				size = 4
			}
			if int(size) > len(data)-i {
				return nil, errors.New("truncated protobuf")
			}
			out = append(out, pfield{int(tag >> 3), true, 0, data[i : i+int(size)]})
			i += int(size)
		default:
			return nil, errors.New("unsupported protobuf wire type")
		}
	}
	return out, nil
}

// fieldsMap 保留每个字段最后一次出现(与 Python dict(fields(...)) 语义一致)
func fieldsMap(data []byte) (map[int]pfield, error) {
	fs, err := parseFields(data)
	if err != nil {
		return nil, err
	}
	m := map[int]pfield{}
	for _, f := range fs {
		m[f.num] = f
	}
	return m, nil
}

// ---------------------------------------------------------------- snappy (raw)

func snappyCompress(data []byte) []byte {
	out := varint(uint64(len(data)))
	for i := 0; i < len(data); i += 60 {
		end := i + 60
		if end > len(data) {
			end = len(data)
		}
		out = append(out, byte((end-i-1)*4))
		out = append(out, data[i:end]...)
	}
	return out
}

func snappyDecompress(data []byte) ([]byte, error) {
	size, i, err := readVarint(data, 0)
	if err != nil {
		return nil, err
	}
	if size > 8*1024*1024 {
		return nil, errors.New("snappy message too large")
	}
	out := make([]byte, 0, size)
	take := func(n int) ([]byte, error) {
		if n > len(data)-i {
			return nil, errors.New("truncated snappy")
		}
		chunk := data[i : i+n]
		i += n
		return chunk, nil
	}
	for i < len(data) {
		tag, err := take(1)
		if err != nil {
			return nil, err
		}
		switch tag[0] & 3 {
		case 0:
			n := int(tag[0] >> 2)
			if n >= 60 {
				b, err := take(n - 59)
				if err != nil {
					return nil, err
				}
				n = 0
				for j, v := range b {
					n |= int(v) << (8 * j)
				}
			}
			lit, err := take(n + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, lit...)
		default:
			var offset, n int
			mode := tag[0] & 3
			if mode == 1 {
				b, err := take(1)
				if err != nil {
					return nil, err
				}
				offset = int(tag[0]>>5)<<8 | int(b[0])
				n = int(tag[0]>>2)&7 + 4
			} else {
				sz := 2
				if mode == 3 {
					sz = 4
				}
				b, err := take(sz)
				if err != nil {
					return nil, err
				}
				for j, v := range b {
					offset |= int(v) << (8 * j)
				}
				n = int(tag[0]>>2) + 1
			}
			if offset <= 0 || offset > len(out) || len(out)+n > int(size) {
				return nil, errors.New("invalid snappy copy")
			}
			for j := 0; j < n; j++ {
				out = append(out, out[len(out)-offset])
			}
		}
		if len(out) > int(size) {
			return nil, errors.New("snappy length exceeded")
		}
	}
	if len(out) != int(size) {
		return nil, errors.New("snappy length mismatch")
	}
	return out, nil
}

// ---------------------------------------------------------------- 自定义 128-bit 曲线 ECDH

var (
	ecP, _ = new(big.Int).SetString("FFFFFFFDFFFFFFFFFFFFFFFFFFFFFFFF", 16)
	ecN, _ = new(big.Int).SetString("FFFFFFFE0000000075A30D1B9038A115", 16)
	ecGx, _ = new(big.Int).SetString("161FF7528B899B2D0C28607CA52C5B86", 16)
	ecGy, _ = new(big.Int).SetString("CF5AC8395BAFEB13C02DA292DDED7A83", 16)
	ecB, _ = new(big.Int).SetString("E87579C11079F43DD824993C2CEE5ED3", 16)
	ecG     = &ecPoint{ecGx, ecGy}
	three   = big.NewInt(3)
)

type ecPoint struct{ x, y *big.Int } // nil 表示无穷远点

func mod(v *big.Int) *big.Int {
	r := new(big.Int).Mod(v, ecP)
	return r
}

func pointAdd(a, b *ecPoint) *ecPoint {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	// x 相等且 y 互逆 → 无穷远
	sum := new(big.Int).Add(a.y, b.y)
	if a.x.Cmp(b.x) == 0 && mod(sum).Sign() == 0 {
		return nil
	}
	var slope *big.Int
	if a == b {
		num := new(big.Int).Mul(a.x, a.x)
		num.Mul(num, three)
		num.Sub(num, three)
		den := new(big.Int).Lsh(a.y, 1)
		slope = mod(new(big.Int).Mul(mod(num), new(big.Int).ModInverse(mod(den), ecP)))
	} else {
		num := new(big.Int).Sub(b.y, a.y)
		den := new(big.Int).Sub(b.x, a.x)
		slope = mod(new(big.Int).Mul(mod(num), new(big.Int).ModInverse(mod(den), ecP)))
	}
	z := mod(new(big.Int).Sub(new(big.Int).Mul(slope, slope), new(big.Int).Add(a.x, b.x)))
	y := mod(new(big.Int).Sub(new(big.Int).Mul(slope, new(big.Int).Sub(a.x, z)), a.y))
	return &ecPoint{z, y}
}

func pointMul(k *big.Int, p *ecPoint) *ecPoint {
	var out *ecPoint
	kCopy := new(big.Int).Set(k)
	for kCopy.Sign() > 0 {
		if kCopy.Bit(0) == 1 {
			out = pointAdd(out, p)
		}
		p = pointAdd(p, p)
		kCopy.Rsh(kCopy, 1)
	}
	return out
}

func sharedKey(k *big.Int, public string) ([]byte, error) {
	if len(public) != 66 || public[:2] != "04" {
		return nil, errors.New("invalid server public key")
	}
	x, ok1 := new(big.Int).SetString(public[2:34], 16)
	y, ok2 := new(big.Int).SetString(public[34:66], 16)
	if !ok1 || !ok2 || x.Sign() < 0 || x.Cmp(ecP) >= 0 || y.Sign() < 0 || y.Cmp(ecP) >= 0 {
		return nil, errors.New("invalid server public key")
	}
	// y² - x³ + 3x - B ≡ 0 (mod P)
	lhs := new(big.Int).Mul(y, y)
	x3 := new(big.Int).Mul(x, x)
	x3.Mul(x3, x)
	lhs.Sub(lhs, x3)
	lhs.Add(lhs, new(big.Int).Mul(three, x))
	lhs.Sub(lhs, ecB)
	if mod(lhs).Sign() != 0 {
		return nil, errors.New("server public key is not on curve")
	}
	p := pointMul(k, &ecPoint{x, y})
	if p == nil {
		return nil, errors.New("invalid shared point")
	}
	return []byte(fmt.Sprintf("%032X", p.x)), nil
}

// ---------------------------------------------------------------- WeType 客户端

const (
	wetypeHost  = "wetype.weixin.qq.com"
	signKey     = "zN7rB3bL4pO8jW1o"
	cmdDH       = 2147483646
	cmdUIN      = 0x7FFFFDFD
)

var bootKey = []byte("D4Y5U3Y2M0C0T7N4P1P7O2N6E1I2Y1U6")

type Client struct {
	ws     *wsClient
	key    []byte
	task   int
	uin    string
	device string
}

func NewClient() (*Client, error) {
	ws, err := newWSClient(wetypeHost, map[string]string{"Sec-WebSocket-Protocol": "wxws_pb"}, ioTimeout)
	if err != nil {
		return nil, err
	}
	return &Client{ws: ws, uin: "0"}, nil
}

func randomLower(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = letters[b[i]%26]
	}
	return string(b)
}

func (c *Client) roundtrip(path string, body []byte, cmd int, compression string, token string) ([]byte, map[string]string, error) {
	special := cmd == cmdDH || cmd == cmdUIN
	if !special {
		c.task++
	}
	task := cmd
	if !special {
		task = c.task
	}
	ts := strconv.FormatInt(time.Now().UnixNano()/1000000, 10)
	trace := randomLower(16)
	md5sum := md5.Sum(body)
	digest := strings.ToUpper(hex.EncodeToString(md5sum[:]))
	values := []string{"5", "2.2.3(657)", "2"}
	switch cmd {
	case cmdDH:
		values = append(values, ts, digest, trace, strconv.Itoa(cmd))
	case cmdUIN:
		values = append(values, ts, digest, token, trace, strconv.Itoa(cmd))
	default:
		values = append(values, strconv.Itoa(cmd), "0", ts, digest, c.uin, trace, strconv.Itoa(task))
	}
	headers := map[string]string{"Kb-Uin": c.uin}
	if token != "" {
		headers["Kb-GenUinToken"] = token
	}
	if special {
		headers["Kb-DeviceCodeRestrictionV2"] = "1"
	}
	if cmd != cmdDH {
		if c.key != nil {
			headers["Kb-SharedKeySuffix"] = string(c.key[len(c.key)-4:])
		}
		headers["Kb-CmdId"] = strconv.Itoa(cmd)
		headers["Kb-SubCmdId"] = "0"
	}
	sign := sha256.Sum256([]byte(strings.Join(values, "") + signKey))
	for k, v := range map[string]string{
		"Kb-OsType": "5", "Kb-Version": "2.2.3(657)", "Kb-SystemVersion": "27.0.0",
		"Kb-PackageType": "3", "Use_DebugNet": "0", "Kb-TimeStamp": ts,
		"Kb-BodyMd5": digest, "Kb-TraceId": trace, "Kb-TaskId": strconv.Itoa(task),
		"Kb-Scene": "2", "Kb-Sign": strings.ToUpper(hex.EncodeToString(sign[:])),
		"Content-Length": strconv.Itoa(len(body)), "Kb-CompressionType": compression,
		"Content-Type": "application/octet-stream", "HOST": wetypeHost,
	} {
		headers[k] = v
	}
	http := field(1, "POST")
	http = append(http, field(3, path)...)
	http = append(http, field(4, "")...)
	for k, v := range headers {
		entry := field(1, k)
		entry = append(entry, field(2, v)...)
		http = append(http, field(5, entry)...)
	}
	http = append(http, field(6, body)...)

	msg := field(1, 0)
	msg = append(msg, field(2, 0)...)
	msg = append(msg, field(3, task)...)
	msg = append(msg, field(5, http)...)
	if err := c.ws.sendBinary(msg); err != nil {
		return nil, nil, err
	}
	raw, err := c.ws.recvBinary()
	if err != nil {
		return nil, nil, err
	}
	outer, err := fieldsMap(raw)
	if err != nil {
		return nil, nil, err
	}
	f4, ok := outer[4]
	if !ok || !f4.isBytes {
		return nil, nil, errors.New("WeType response missing body container")
	}
	resp, err := parseFields(f4.b)
	if err != nil {
		return nil, nil, err
	}
	parsed := map[int]pfield{}
	for _, f := range resp {
		parsed[f.num] = f
	}
	if st, ok := parsed[2]; !ok || st.v != 200 {
		code := uint64(0)
		if ok {
			code = st.v
		}
		return nil, nil, fmt.Errorf("WeType HTTP status %d at %s", code, path)
	}
	respHeaders := map[string]string{}
	for _, f := range resp {
		if f.num == 5 || !f.isBytes {
			continue
		}
		if kv, err := fieldsMap(f.b); err == nil {
			if k, ok1 := kv[1]; ok1 && k.isBytes {
				if v, ok2 := kv[2]; ok2 && v.isBytes {
					respHeaders[string(k.b)] = string(v.b)
				}
			}
		}
	}
	var respBody []byte
	if b, ok := parsed[5]; ok && b.isBytes {
		respBody = b.b
	}
	return respBody, respHeaders, nil
}

func (c *Client) exchange(register bool) (string, string, error) {
	nm1 := new(big.Int).Sub(ecN, big.NewInt(1))
	k, err := rand.Int(rand.Reader, nm1)
	if err != nil {
		return "", "", err
	}
	k.Add(k, big.NewInt(1))
	p := pointMul(k, ecG)
	if p == nil {
		return "", "", errors.New("invalid local ECDH key")
	}
	public := fmt.Sprintf("04%032X%032X", p.x, p.y)
	req := field(1, public)
	req = append(req, field(2, public)...)
	req = append(req, field(4, c.device)...)
	req = append(req, field(7, "")...)
	req = append(req, field(8, "Mac16,12")...)
	req = append(req, field(9, "")...)
	if register {
		req = append(req, field(3, 1)...)
	} else {
		uin, _ := strconv.ParseUint(c.uin, 10, 64)
		req = append(req, field(5, uin)...)
	}
	enc, err := aesCrypt(bootKey, req, true)
	if err != nil {
		return "", "", err
	}
	body, _, err := c.roundtrip("/oauth_pubkey_v2", enc, cmdDH, "1", "")
	if err != nil {
		return "", "", err
	}
	plain, err := aesCrypt(bootKey, body, false)
	if err != nil {
		return "", "", err
	}
	reply, err := fieldsMap(plain)
	if err != nil {
		return "", "", err
	}
	server := string(reply[2].b)
	c.key, err = sharedKey(k, server)
	if err != nil {
		return "", "", err
	}
	token := ""
	if t, ok := reply[4]; ok && t.isBytes {
		token = string(t.b)
	}
	return server, token, nil
}

func (c *Client) Handshake() error {
	if _, _, err := c.roundtrip("/timestamp", nil, 0, "1", ""); err != nil {
		return err
	}
	cached := false
	if data, err := os.ReadFile(credPath); err == nil {
		var identity map[string]interface{}
		if json.Unmarshal(data, &identity) == nil {
			device, _ := identity["device"].(string)
			var uin int64
			switch v := identity["uin"].(type) {
			case float64:
				uin = int64(v)
			case string:
				uin, _ = strconv.ParseInt(v, 10, 64)
			}
			if device != "" && uin > 0 {
				c.device = device
				c.uin = strconv.FormatInt(uin, 10)
				if _, _, err := c.exchange(false); err == nil {
					cached = true
				}
			}
		}
	}
	if !cached {
		c.uin = "0"
		body := "MAC" + strings.Repeat("0", 17-len("Mac16,12")) + "Mac16,12" + randomLower(12)
		sum := md5.Sum([]byte(body + signKey))
		c.device = body + strings.ToUpper(hex.EncodeToString(sum[:]))
		server, token, err := c.exchange(true)
		if err != nil {
			return err
		}
		if token == "" {
			return errors.New("WeType registration token missing")
		}
		req := field(1, c.device)
		req = append(req, field(2, server)...)
		req = append(req, field(4, token)...)
		enc, err := aesCrypt(c.key, req, true)
		if err != nil {
			return err
		}
		body2, _, err := c.roundtrip("/gen_uin_v2", enc, cmdUIN, "1", token)
		if err != nil {
			return err
		}
		plain, err := aesCrypt(c.key, body2, false)
		if err != nil {
			return err
		}
		reply, err := fieldsMap(plain)
		if err != nil {
			return err
		}
		uin := int64(0)
		if f, ok := reply[2]; ok {
			uin = int64(f.v)
		}
		if uin == 0 {
			return errors.New("WeType registration failed")
		}
		c.uin = strconv.FormatInt(uin, 10)
		if err := os.MkdirAll(filepath.Dir(credPath), 0o755); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]interface{}{"device": c.device, "uin": uin})
		if err := os.WriteFile(credPath, payload, 0o600); err != nil {
			return err
		}
	}
	enc, err := aesCrypt(c.key, nil, true)
	if err != nil {
		return err
	}
	_, _, err = c.roundtrip("/api_v2", enc, 8074, "1", "")
	return err
}

// voice 返回 (raw 临时文本, final 定稿文本)
func (c *Client) voice(voiceID string, opus []byte, seq int, total int, end bool) (string, string, error) {
	req := field(2, voiceID)
	req = append(req, field(5, 5)...)
	req = append(req, field(7, seq)...)
	req = append(req, field(22, 1)...)
	req = append(req, field(23, 1)...)
	req = append(req, field(24, 1)...)
	if len(opus) > 0 {
		req = append(req, field(4, opus)...)
	}
	if end {
		req = append(req, field(6, 1)...)
	}
	if total > 0 {
		req = append(req, field(11, total)...)
	}
	inner := field(1, req)
	enc, err := aesCrypt(c.key, snappyCompress(inner), true)
	if err != nil {
		return "", "", err
	}
	body, headers, err := c.roundtrip("/api_v2", enc, 4548, "2", "")
	if err != nil {
		return "", "", err
	}
	plain, err := aesCrypt(c.key, body, false)
	if err != nil {
		return "", "", err
	}
	ct := headers["Kb-CompressionType"]
	if ct == "" {
		ct = headers["CompressionType"]
	}
	if ct == "2" {
		if plain, err = snappyDecompress(plain); err != nil {
			return "", "", err
		}
	}
	reply, err := fieldsMap(plain)
	if err != nil {
		return "", "", err
	}
	if f, ok := reply[1]; ok && f.isBytes {
		if reply, err = fieldsMap(f.b); err != nil {
			return "", "", err
		}
	}
	rawText, finalText := "", ""
	if f, ok := reply[4]; ok && f.isBytes {
		rawText = string(f.b)
	}
	if f, ok := reply[14]; ok && f.isBytes {
		finalText = string(f.b)
	}
	return rawText, finalText, nil
}

// ---------------------------------------------------------------- 应用逻辑

func logMsg(msg string) {
	os.MkdirAll(cacheDir, 0o755)
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

// defaultMicSource 返回默认录音源(麦克风)名称, 绝不返回 monitor(系统声音)源
func defaultMicSource() string {
	p, err := exec.LookPath("pactl")
	if err != nil {
		return ""
	}
	if out, err := exec.Command(p, "get-default-source").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" && !strings.HasSuffix(name, ".monitor") {
			return name
		}
	}
	out, err := exec.Command(p, "list", "short", "sources").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && !strings.HasSuffix(f[1], ".monitor") {
			return f[1]
		}
	}
	return ""
}

func pickRecorder() []string {
	src := defaultMicSource()
	if p, err := exec.LookPath("pw-record"); err == nil {
		args := []string{p, "--rate", "16000", "--channels", "1", "--format", "s16", "--latency", "20ms"}
		if src != "" {
			args = append(args, "--target", src)
		}
		return append(args, "-")
	}
	if p, err := exec.LookPath("parec"); err == nil {
		args := []string{p, "--rate=16000", "--channels=1", "--format=s16le", "--latency-msec=20"}
		if src != "" {
			args = append(args, "-d", src)
		}
		return args
	}
	if p, err := exec.LookPath("arecord"); err == nil {
		return []string{p, "-q", "-f", "S16_LE", "-r", "16000", "-c", "1", "-t", "raw"}
	}
	return nil
}

// typeText 上屏: 全部走剪贴板粘贴(不丢字), 终端用 Ctrl+Shift+V, 普通窗口用 Ctrl+V;
// 无 xclip 或粘贴失败时回退 xdotool 模拟按键
// WETYPE_X11_TYPE=type 可强制只用模拟按键
func typeText(text string) error {
	if os.Getenv("WETYPE_X11_TYPE") != "type" {
		keys := "ctrl+v"
		if isTerminal(activeWindowClass()) {
			keys = "ctrl+shift+v"
		}
		if err := pasteClipboard(text, keys); err == nil {
			return nil
		} else {
			logMsg("warn: 剪贴板粘贴失败, 回退模拟按键: " + err.Error())
		}
	}
	return typeKeystrokes(text)
}

func typeKeystrokes(text string) error {
	p, err := exec.LookPath("xdotool")
	if err != nil {
		return errors.New("找不到 xdotool")
	}
	return exec.Command(p, "type", "--clearmodifiers", "--delay", "25", "--", text).Run()
}

// activeWindowClass 解析焦点窗口 WM_CLASS 的 class 部分
// (xdotool 没有 getwindowclassname 命令, 需配合 xprop)
func activeWindowClass() string {
	xdotool, err := exec.LookPath("xdotool")
	if err != nil {
		return ""
	}
	xprop, err := exec.LookPath("xprop")
	if err != nil {
		return ""
	}
	out, err := exec.Command(xdotool, "getactivewindow").Output()
	if err != nil {
		return ""
	}
	out, err = exec.Command(xprop, "-id", strings.TrimSpace(string(out)), "WM_CLASS").Output()
	if err != nil {
		return ""
	}
	// 形如: WM_CLASS(STRING) = "instance", "class"
	s := string(out)
	i := strings.LastIndex(s, "\"")
	if i <= 0 {
		return ""
	}
	j := strings.LastIndex(s[:i], "\"")
	if j < 0 {
		return ""
	}
	return s[j+1 : i]
}

// 精确匹配(不区分大小写), 避免子串误判(如 "st" 会误中 "Steam")
var terminalClasses = map[string]bool{
	"st-256color": true, "st": true, "xterm": true, "urxvt": true,
	"rxvt-unicode": true, "alacritty": true, "kitty": true, "wezterm": true,
	"gnome-terminal-server": true, "konsole": true, "terminator": true,
	"tilix": true, "xfce4-terminal": true, "lxterminal": true,
	"mate-terminal": true, "cool-retro-term": true, "contour": true,
}

func isTerminal(class string) bool {
	return terminalClasses[strings.ToLower(class)]
}

// pasteClipboard: 暂存旧剪贴板 → 写入识别结果 → 模拟粘贴键 → 恢复旧剪贴板
// keys 依窗口类型不同: 普通窗口 ctrl+v, 终端 ctrl+shift+v
// 剪贴板工具自动探测: 优先 xclip, 备选 xsel(两者操作同一 X11 CLIPBOARD 选区)
type clipTool struct {
	path    string
	getArgs []string
	setArgs []string
}

func findClipTool() *clipTool {
	if p, err := exec.LookPath("xclip"); err == nil {
		return &clipTool{p, []string{"-o", "-selection", "clipboard"}, []string{"-selection", "clipboard", "-in"}}
	}
	if p, err := exec.LookPath("xsel"); err == nil {
		return &clipTool{p, []string{"--clipboard", "--output"}, []string{"--clipboard", "--input"}}
	}
	return nil
}

func pasteClipboard(text, keys string) error {
	clip := findClipTool()
	if clip == nil {
		return errors.New("找不到 xclip/xsel")
	}
	xdotool, err := exec.LookPath("xdotool")
	if err != nil {
		return err
	}
	old, _ := exec.Command(clip.path, clip.getArgs...).Output() // 空剪贴板报错属正常
	set := exec.Command(clip.path, clip.setArgs...)
	set.Stdin = strings.NewReader(text)
	if err := set.Run(); err != nil {
		return err
	}
	time.Sleep(120 * time.Millisecond) // 等 selection 就绪
	if err := exec.Command(xdotool, "key", "--clearmodifiers", keys).Run(); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond) // 等应用取走内容
	if len(old) > 0 {
		restore := exec.Command(clip.path, clip.setArgs...)
		restore.Stdin = bytes.NewReader(old)
		restore.Run()
	}
	return nil
}

// WETYPE_X11_MUTE=1 时, 录音期间临时静音系统输出(防止外放声音被麦克风收录)

var sinkMutedByUs bool

func muteSink() {
	if os.Getenv("WETYPE_X11_MUTE") != "1" {
		return
	}
	p, err := exec.LookPath("pactl")
	if err != nil {
		return
	}
	// LC_ALL=C: 避免输出被本地化("Mute: yes" vs "静音: 是")
	cmd := exec.Command(p, "get-sink-mute", "@DEFAULT_SINK@")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if out, err := cmd.Output(); err == nil && strings.Contains(string(out), "yes") {
		return // 本来就是静音, 不动也不负责恢复
	}
	if exec.Command(p, "set-sink-mute", "@DEFAULT_SINK@", "1").Run() == nil {
		sinkMutedByUs = true
	}
}

func unmuteSink() {
	if !sinkMutedByUs {
		return
	}
	if p, err := exec.LookPath("pactl"); err == nil {
		exec.Command(p, "set-sink-mute", "@DEFAULT_SINK@", "0").Run()
	}
	sinkMutedByUs = false
}

func runSession() int {
	os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644)
	defer os.Remove(pidFile)

	sigCh := make(chan os.Signal, 4)
	signalNotify(sigCh)
	finish := false
	cancel := false

	recorder := pickRecorder()
	if recorder == nil {
		logMsg("error: 找不到录音命令 (pw-record/parec/arecord)")
		return 1
	}
	cmd := exec.Command(recorder[0], recorder[1:]...)
	cmd.Stderr = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logMsg("error: " + err.Error())
		return 1
	}
	if err := cmd.Start(); err != nil {
		logMsg("error: 录音命令启动失败: " + err.Error())
		return 1
	}
	muteSink()
	defer unmuteSink() // 兜底: 任何异常退出都恢复声音
	killRecorder := func() {
		cmd.Process.Kill()
		cmd.Wait()
		unmuteSink() // 录音一停立刻恢复, 不耽误定稿等待期间听声音
	}

	// 录音读取协程: 录音进程一启动就开始读, 避免握手期间管道/PipeWire 缓冲溢出丢开头;
	// 大缓冲 channel 保证停止信号到来时积压的数据一个都不丢
	recvCh := make(chan []byte, 512) // 512 × 640B ≈ 10 秒音频
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		for {
			buf := make([]byte, 640)
			if _, err := io.ReadFull(stdout, buf); err != nil {
				return
			}
			recvCh <- buf
		}
	}()

	client, err := NewClient()
	if err != nil {
		killRecorder()
		logMsg("error: " + err.Error())
		return 1
	}
	defer client.ws.abort()

	if err := client.Handshake(); err != nil {
		killRecorder()
		logMsg("error: handshake: " + err.Error())
		return 1
	}
	encoder, err := NewOpusEncoder()
	if err != nil {
		killRecorder()
		logMsg("error: " + err.Error())
		return 1
	}
	defer encoder.Close()

	voiceID := strings.ReplaceAll(newUUID(), "-", "")
	seq, total := 0, 0
	text, polished := "", ""
	var packets [][]byte
	var pcm []byte

	send := func(end bool, poll bool) error {
		var framed []byte
		number := 0
		if !poll {
			seq++
			number = seq
			if seq == 1 {
				framed = []byte("#!OPUS_RAW_V1")
				framed = append(framed, 2, 1, 0)
			}
			for _, p := range packets {
				var sz [2]byte
				binary.LittleEndian.PutUint16(sz[:], uint16(len(p)))
				framed = append(framed, sz[:]...)
				framed = append(framed, p...)
			}
			packets = packets[:0]
			total += len(framed)
		}
		raw, final, err := client.voice(voiceID, framed, number, total, end)
		if err != nil {
			return err
		}
		if raw != "" && raw != text {
			text = raw
			logMsg("partial: " + text)
		}
		if end && final != "" {
			polished = final
		}
		return nil
	}

	// 录音循环: 读 PCM → 编码 → 每 6 包(120ms)上传
	deadline := time.Now().Add(time.Duration(maxSecs * float64(time.Second)))

recordLoop:
	for !finish && time.Now().Before(deadline) {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGUSR1 {
				finish = true
			} else {
				cancel, finish = true, true
			}
		case buf := <-recvCh:
			pcm = append(pcm, buf...)
			for len(pcm) >= 640 {
				pkt, err := encoder.Encode(pcm[:640])
				if err != nil {
					logMsg("error: " + err.Error())
					break recordLoop
				}
				packets = append(packets, pkt)
				pcm = pcm[640:]
				if len(packets) == 6 {
					if err := send(false, false); err != nil {
						logMsg("error: " + err.Error())
						break recordLoop
					}
				}
			}
		case <-doneCh:
			logMsg("error: 录音进程意外结束")
			break recordLoop
		}
	}

	killRecorder()
	// 排干协程 channel 里积压的音频(停止时最多可压 ~10s, 这就是"后半句"), 再读管道残余
	for done := false; !done; {
		select {
		case buf := <-recvCh:
			pcm = append(pcm, buf...)
		case <-doneCh:
			done = true
		}
	}
	for {
		select {
		case buf := <-recvCh:
			pcm = append(pcm, buf...)
		default:
			goto drained
		}
	}
drained:
	if rest, err := io.ReadAll(stdout); err == nil {
		pcm = append(pcm, rest...)
	}

	if cancel {
		logMsg("cancelled")
		return 0
	}

	for len(pcm) >= 640 {
		pkt, err := encoder.Encode(pcm[:640])
		if err != nil {
			break
		}
		packets = append(packets, pkt)
		pcm = pcm[640:]
	}
	if len(pcm) > 0 {
		padded := append(append([]byte{}, pcm...), make([]byte, 640-len(pcm))...)
		if pkt, err := encoder.Encode(padded); err == nil {
			packets = append(packets, pkt)
		}
	}
	if seq == 0 && len(packets) == 0 {
		logMsg("warn: 未录到声音")
		return 0
	}

	if err := send(true, false); err != nil {
		// 定稿上传失败但已有临时文本时, 保留最新文本
		if text == "" {
			logMsg("error: " + err.Error())
			return 1
		}
		logMsg("warn: finish send: " + err.Error())
	} else {
		pollDeadline := time.Now().Add(time.Duration(finishGrace * float64(time.Second)))
		for i := 0; i < 8 && polished == "" && time.Now().Before(pollDeadline); i++ {
			time.Sleep(250 * time.Millisecond)
			if polished != "" || !time.Now().Before(pollDeadline) {
				break
			}
			if err := send(true, true); err != nil {
				break
			}
		}
	}

	result := polished
	if result == "" {
		result = text
	}
	if result != "" {
		if err := typeText(result); err != nil {
			logMsg("error: type: " + err.Error())
			return 1
		}
		logMsg("final: " + result)
	} else {
		logMsg("warn: 未识别到内容")
	}
	return 0
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func signalNotify(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGUSR1, syscall.SIGTERM, syscall.SIGINT)
}

func readPID() int {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0
	}
	return pid
}

func toggle() int {
	if pid := readPID(); pid != 0 {
		syscall.Kill(pid, syscall.SIGUSR1)
		fmt.Println("已请求结束录音并上屏")
		return 0
	}
	os.MkdirAll(cacheDir, 0o755)
	lf, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer lf.Close()
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "--record")
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("已开始录音")
	return 0
}

func cancelSession() int {
	pid := readPID()
	if pid == 0 {
		fmt.Println("当前没有录音会话")
		return 1
	}
	syscall.Kill(pid, syscall.SIGTERM)
	fmt.Println("已请求取消")
	return 0
}

func check() int {
	ok := true
	fmt.Println("opus: 内联静态链接 (libopus)")
	fmt.Println("AES/TLS/ECDH: Go 标准库")
	if r := pickRecorder(); r != nil {
		fmt.Println("录音命令:", strings.Join(r, " "))
	} else {
		fmt.Println("录音命令: 未找到 pw-record/parec/arecord")
		ok = false
	}
	for _, name := range []string{"xdotool", "xprop"} {
		if p, err := exec.LookPath(name); err == nil {
			fmt.Printf("%s: %s\n", name, p)
		} else {
			fmt.Printf("%s: 未找到\n", name)
			if name == "xdotool" || name == "xprop" {
				ok = false
			}
		}
	}
	if c := findClipTool(); c != nil {
		fmt.Println("剪贴板工具:", c.path)
	} else {
		fmt.Println("剪贴板工具: 未找到 xclip/xsel (将回退模拟按键, 中文可能丢字)")
	}
	if strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") {
		fmt.Println("警告: 当前是 Wayland 会话, xdotool 仅对 X11/XWayland 窗口有效")
	}
	if !ok {
		return 1
	}
	return 0
}

func handshakeTest() int {
	client, err := NewClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer client.ws.abort()
	t0 := time.Now()
	if err := client.Handshake(); err != nil {
		fmt.Fprintln(os.Stderr, "握手失败:", err)
		return 1
	}
	fmt.Printf("握手成功 (uin=%s, %.2fs)\n", client.uin, time.Since(t0).Seconds())
	return 0
}

const usage = `wetype-x11 — 单文件微信输入法(WeType)流式语音转文字工具(X11)

用法:
    wetype-x11             切换: 第一次按开始录音, 第二次按结束并上屏
    wetype-x11 --record    内部使用: 录音会话进程
    wetype-x11 --cancel    取消当前录音
    wetype-x11 --check     依赖自检
    wetype-x11 --handshake 测试与 wetype.weixin.qq.com 的连通性
`

func main() {
	arg := ""
	if len(os.Args) > 1 {
		arg = os.Args[1]
	}
	switch arg {
	case "--record":
		os.Exit(runSession())
	case "--cancel":
		os.Exit(cancelSession())
	case "--check":
		os.Exit(check())
	case "--handshake":
		os.Exit(handshakeTest())
	case "--wclass":
		cls := activeWindowClass()
		fmt.Printf("class=%q terminal=%v\n", cls, isTerminal(cls))
	case "-h", "--help":
		fmt.Print(usage)
	default:
		os.Exit(toggle())
	}
}
