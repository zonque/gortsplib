package gortsplib

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/aler9/gortsplib/pkg/base"
)

// Play writes a PLAY request and reads a Response.
// This can be called only after Setup().
func (c *ClientConn) Play() (*base.Response, error) {
	err := c.checkState(map[clientConnState]struct{}{
		clientConnStatePrePlay: {},
	})
	if err != nil {
		return nil, err
	}

	res, err := c.Do(&base.Request{
		Method: base.Play,
		URL:    c.streamURL,
	})
	if err != nil {
		return nil, err
	}

	if res.StatusCode != base.StatusOK {
		return nil, fmt.Errorf("bad status code: %d (%s)", res.StatusCode, res.StatusMessage)
	}

	return res, nil
}

func (c *ClientConn) backgroundPlayUDP(onFrameDone chan error) {
	defer close(c.backgroundDone)

	var returnError error

	defer func() {
		for trackID := range c.udpRtpListeners {
			c.udpRtpListeners[trackID].stop()
			c.udpRtcpListeners[trackID].stop()
		}

		onFrameDone <- returnError
	}()

	// open the firewall by sending packets to the counterpart
	for trackID := range c.udpRtpListeners {
		c.udpRtpListeners[trackID].write(
			[]byte{0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})

		c.udpRtcpListeners[trackID].write(
			[]byte{0x80, 0xc9, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00})
	}

	for trackID := range c.udpRtpListeners {
		c.udpRtpListeners[trackID].start()
		c.udpRtcpListeners[trackID].start()
	}

	// disable deadline
	c.nconn.SetReadDeadline(time.Time{})

	readerDone := make(chan error)
	go func() {
		for {
			var res base.Response
			err := res.Read(c.br)
			if err != nil {
				readerDone <- err
				return
			}
		}
	}()

	reportTicker := time.NewTicker(clientReceiverReportPeriod)
	defer reportTicker.Stop()

	keepaliveTicker := time.NewTicker(clientUDPKeepalivePeriod)
	defer keepaliveTicker.Stop()

	checkStreamTicker := time.NewTicker(clientUDPCheckStreamPeriod)
	defer checkStreamTicker.Stop()

	for {
		select {
		case <-c.backgroundTerminate:
			c.nconn.SetReadDeadline(time.Now())
			<-readerDone
			returnError = fmt.Errorf("terminated")
			return

		case <-reportTicker.C:
			now := time.Now()
			for trackID := range c.rtcpReceivers {
				r := c.rtcpReceivers[trackID].Report(now)
				c.udpRtcpListeners[trackID].write(r)
			}

		case <-keepaliveTicker.C:
			_, err := c.Do(&base.Request{
				Method: func() base.Method {
					// the vlc integrated rtsp server requires GET_PARAMETER
					if c.getParameterSupported {
						return base.GetParameter
					}
					return base.Options
				}(),
				// use the stream path, otherwise some cameras do not reply
				URL:          c.streamURL,
				SkipResponse: true,
			})
			if err != nil {
				c.nconn.SetReadDeadline(time.Now())
				<-readerDone
				returnError = err
				return
			}

		case <-checkStreamTicker.C:
			now := time.Now()

			for _, lastUnix := range c.udpLastFrameTimes {
				last := time.Unix(atomic.LoadInt64(lastUnix), 0)

				if now.Sub(last) >= c.c.ReadTimeout {
					c.nconn.SetReadDeadline(time.Now())
					<-readerDone
					returnError = fmt.Errorf("no packets received recently (maybe there's a firewall/NAT in between)")
					return
				}
			}

		case err := <-readerDone:
			returnError = err
			return
		}
	}
}

func (c *ClientConn) backgroundPlayTCP(onFrameDone chan error) {
	defer close(c.backgroundDone)

	var returnError error

	defer func() {
		onFrameDone <- returnError
	}()

	readerDone := make(chan error)
	go func() {
		for {
			frame := base.InterleavedFrame{
				Content: c.tcpFrameBuffer.Next(),
			}
			err := frame.Read(c.br)
			if err != nil {
				readerDone <- err
				return
			}

			c.rtcpReceivers[frame.TrackID].ProcessFrame(time.Now(), frame.StreamType, frame.Content)

			c.readCB(frame.TrackID, frame.StreamType, frame.Content)
		}
	}()

	reportTicker := time.NewTicker(clientReceiverReportPeriod)
	defer reportTicker.Stop()

	// for some reason, SetReadDeadline() must always be called in the same
	// goroutine, otherwise Read() freezes.
	// therefore, we call it with a ticker.
	deadlineTicker := time.NewTicker(1 * time.Second)
	defer deadlineTicker.Stop()

	for {
		select {
		case <-deadlineTicker.C:
			c.nconn.SetReadDeadline(time.Now().Add(c.c.ReadTimeout))

		case <-c.backgroundTerminate:
			c.nconn.SetReadDeadline(time.Now())
			<-readerDone
			returnError = fmt.Errorf("terminated")
			return

		case <-reportTicker.C:
			now := time.Now()
			for trackID := range c.rtcpReceivers {
				r := c.rtcpReceivers[trackID].Report(now)
				c.nconn.SetWriteDeadline(time.Now().Add(c.c.WriteTimeout))
				frame := base.InterleavedFrame{
					TrackID:    trackID,
					StreamType: StreamTypeRtcp,
					Content:    r,
				}
				frame.Write(c.bw)
			}

		case err := <-readerDone:
			returnError = err
			return
		}
	}
}

// OnFrame sets a callback that is called when a frame is received.
// it returns a channel that is called when the reading stops.
// This can be called only after Play().
func (c *ClientConn) OnFrame(cb func(int, StreamType, []byte)) chan error {
	// channel is buffered, since listening to it is not mandatory
	onFrameDone := make(chan error, 1)

	err := c.checkState(map[clientConnState]struct{}{
		clientConnStatePrePlay: {},
	})
	if err != nil {
		onFrameDone <- err
		return onFrameDone
	}

	c.state = clientConnStatePlay
	c.readCB = cb
	c.backgroundTerminate = make(chan struct{})
	c.backgroundDone = make(chan struct{})

	if *c.streamProtocol == StreamProtocolUDP {
		go c.backgroundPlayUDP(onFrameDone)
	} else {
		go c.backgroundPlayTCP(onFrameDone)
	}

	return onFrameDone
}
