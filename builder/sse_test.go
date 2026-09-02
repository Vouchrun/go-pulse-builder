package builder

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

// lighthouse-style versioned payload_attributes event JSON (single data line).
const paEventJSON = `{"version":"capella","data":{"proposal_slot":"8491993","parent_block_number":"28819856","parent_block_root":"0xcf8e0d4e9587369b2301d0790347320302cc0943d5a1884560367e8208d920f2","parent_block_hash":"0x9a2fefd2fdb57f74993c7780ea5b9030d2897b615b89f808011ca5aebed54eaf","payload_attributes":{"timestamp":"1234567890","prev_randao":"0xaa8e0d4e9587369b2301d0790347320302cc0943d5a1884560367e8208d920f2","suggested_fee_recipient":"0x1111111111111111111111111111111111111111","parent_beacon_block_root":"0xcf8e0d4e9587369b2301d0790347320302cc0943d5a1884560367e8208d920f2","withdrawals":[{"index":"5","validator_index":"10","address":"0x2222222222222222222222222222222222222222","amount":"15640"}]}}}`

const paEventCRLF = "event: payload_attributes\r\ndata: " + paEventJSON + "\r\n\r\n"
const paEventLF = "event: payload_attributes\ndata: " + paEventJSON + "\n\n"

// chunkedReader simulates TCP fragmentation: it hands the underlying data to
// bufio.Reader in awkward, arbitrarily-sized chunks.
type chunkedReader struct {
	data   []byte
	chunks []int
	off    int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.off >= len(c.data) {
		return 0, io.EOF
	}
	n := len(c.data) - c.off
	if len(c.chunks) > 0 {
		want := c.chunks[0]
		c.chunks = c.chunks[1:]
		if want < n {
			n = want
		}
	} else if n > 64 {
		n = 64
	}
	copy(p, c.data[c.off:c.off+n])
	c.off += n
	return n, nil
}

func TestReadSSEEvent(t *testing.T) {
	t.Run("single LF event", func(t *testing.T) {
		ev, err := readSSEEvent(bufio.NewReader(strings.NewReader(paEventLF)))
		require.NoError(t, err)
		require.Equal(t, "payload_attributes", ev.event)
		require.Equal(t, paEventJSON, ev.data)
	})

	t.Run("CRLF event", func(t *testing.T) {
		ev, err := readSSEEvent(bufio.NewReader(strings.NewReader(paEventCRLF)))
		require.NoError(t, err)
		require.Equal(t, "payload_attributes", ev.event)
		require.Equal(t, paEventJSON, ev.data)
	})

	t.Run("chunk-split event buffers until terminator", func(t *testing.T) {
		// Split mid-JSON, including inside a field value.
		chunks := []int{17, 31, 400, 1, 8, 512, 1000}
		r := &chunkedReader{data: []byte(paEventLF), chunks: chunks}
		ev, err := readSSEEvent(bufio.NewReader(r))
		require.NoError(t, err)
		require.Equal(t, "payload_attributes", ev.event)
		require.Equal(t, paEventJSON, ev.data)
	})

	t.Run("keep-alive comments and events after parse", func(t *testing.T) {
		// warp::sse::keep_alive() emits ": ping\n\n" comments (every 15s).
		stream := ": ping\n\n" + paEventLF + ": keep-alive\n\n" + paEventLF
		r := bufio.NewReader(&chunkedReader{data: []byte(stream), chunks: []int{5, 2, 9, 3, 22}})
		// First: the keep-alive comment (no data).
		ev, err := readSSEEvent(r)
		require.NoError(t, err)
		require.Empty(t, ev.data)
		require.Empty(t, ev.event)
		// Second: the real event.
		ev, err = readSSEEvent(r)
		require.NoError(t, err)
		require.Equal(t, "payload_attributes", ev.event)
		require.Equal(t, paEventJSON, ev.data)
		// Third: keep-alive again.
		ev, err = readSSEEvent(r)
		require.NoError(t, err)
		require.Empty(t, ev.data)
		// Fourth: real event again.
		ev, err = readSSEEvent(r)
		require.NoError(t, err)
		require.Equal(t, paEventJSON, ev.data)
	})

	t.Run("multi-line data joined in order", func(t *testing.T) {
		// Split at a token boundary (between "capella" and "data") - the SSE
		// spec joins data: lines with "\n", which is valid JSON whitespace
		// between tokens.
		idx := strings.Index(paEventJSON, ",\"data\"") + 1
		line1, line2 := paEventJSON[:idx], paEventJSON[idx:]
		stream := "event: payload_attributes\ndata: " + line1 + "\ndata: " + line2 + "\n\n"
		ev, err := readSSEEvent(bufio.NewReader(strings.NewReader(stream)))
		require.NoError(t, err)
		require.Equal(t, line1+"\n"+line2, ev.data)
		// The joined payload must still unmarshal (r3labs/sse used to reverse
		// the lines, breaking every multi-line event).
		decoded := new(PayloadAttributesEvent)
		require.NoError(t, json.Unmarshal([]byte(ev.data), decoded))
		require.Equal(t, uint64(8491993), decoded.Data.ProposalSlot)
	})

	t.Run("unrelated topic passes through", func(t *testing.T) {
		ev, err := readSSEEvent(bufio.NewReader(strings.NewReader("event: head\ndata: {\"slot\":\"1\"}\n\n")))
		require.NoError(t, err)
		require.Equal(t, "head", ev.event)
	})

	t.Run("partial event at EOF is dropped, not parsed", func(t *testing.T) {
		truncated := "event: payload_attributes\ndata: " + paEventJSON[:120]
		_, err := readSSEEvent(bufio.NewReader(strings.NewReader(truncated)))
		require.ErrorIs(t, err, io.EOF)
	})

	t.Run("stray blank lines between events are skipped", func(t *testing.T) {
		stream := "\n\n" + paEventLF
		r := bufio.NewReader(strings.NewReader(stream))
		ev, err := readSSEEvent(r)
		require.NoError(t, err)
		require.Equal(t, paEventJSON, ev.data)
	})
}

// TestSubscribePayloadAttributesStream feeds a warp-style stream (keep-alive
// comments interleaved with real events, fragmented into chunks) through the
// full subscription path and asserts the decoded payload attributes arrive.
func TestSubscribePayloadAttributesStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < 2; i++ {
			eventWire := paEventLF
			if i == 1 {
				eventWire = paEventCRLF
			}
			// warp keep-alive comment + real event, chunked.
			w.Write([]byte(": ping\n\n"))
			f.Flush()
			time.Sleep(20 * time.Millisecond)
			w.Write([]byte(eventWire[:40]))
			f.Flush()
			time.Sleep(20 * time.Millisecond)
			w.Write([]byte(eventWire[40:]))
			f.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	client := NewBeaconClient(srv.URL, 32, 10)
	defer client.Stop()
	ch := make(chan types.BuilderPayloadAttributes, 4)

	go client.SubscribeToPayloadAttributesEvents(ch)

	for i := 0; i < 2; i++ {
		select {
		case attrs := <-ch:
			require.Equal(t, uint64(8491993), attrs.Slot)
			require.Equal(t, "0x9a2fefd2fdb57f74993c7780ea5b9030d2897b615b89f808011ca5aebed54eaf", attrs.HeadHash.Hex())
			require.Equal(t, hexutil.Uint64(1234567890), attrs.Timestamp)
			require.Equal(t, "0xaa8e0d4e9587369b2301d0790347320302cc0943d5a1884560367e8208d920f2", attrs.Random.Hex())
			require.Equal(t, "0x1111111111111111111111111111111111111111", attrs.SuggestedFeeRecipient.Hex())
			require.Len(t, attrs.Withdrawals, 1)
			require.Equal(t, uint64(5), attrs.Withdrawals[0].Index)
			require.Equal(t, uint64(10), attrs.Withdrawals[0].Validator)
			require.Equal(t, "0x2222222222222222222222222222222222222222", attrs.Withdrawals[0].Address.Hex())
			require.Equal(t, uint64(15640), attrs.Withdrawals[0].Amount)
			require.Equal(t, "0xcf8e0d4e9587369b2301d0790347320302cc0943d5a1884560367e8208d920f2", attrs.ParentBeaconBlockRoot.Hex())
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for payload attributes event %d", i)
		}
	}
	// Give the keep-alives a moment to confirm they are skipped without
	// blocking or spamming the stream.
	select {
	case attrs := <-ch:
		t.Fatalf("unexpected extra payload attributes event: %+v", attrs)
	case <-time.After(200 * time.Millisecond):
	}
}
