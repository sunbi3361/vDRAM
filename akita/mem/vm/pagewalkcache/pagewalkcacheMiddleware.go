package pagewalkcache

import (
	"log"

	"github.com/sarchlab/akita/v4/sim"
)

type middleware struct {
	*Comp
}

func (m *middleware) Tick() bool {
	madeProgress := m.advanceReads()
	madeProgress = m.processIncoming() || madeProgress
	return madeProgress
}

func (m *middleware) advanceReads() bool {
	madeProgress := false
	for i := range m.reads {
		read := &m.reads[i]
		if read.cycleLeft > 0 {
			read.cycleLeft--
			madeProgress = true
			continue
		}
		if !m.sendLookupRsp(read.req) {
			continue
		}
		m.removeReads = append(m.removeReads, i)
		madeProgress = true
	}

	remaining := m.reads[:0]
	for i, read := range m.reads {
		if !m.markedForRemoval(i) {
			remaining = append(remaining, read)
		}
	}
	m.reads = remaining
	m.removeReads = nil
	return madeProgress
}

func (m *middleware) processIncoming() bool {
	madeProgress := false
	for i := 0; i < m.numReqPerCycle; i++ {
		item := m.topPort.PeekIncoming()
		if item == nil {
			break
		}

		switch msg := item.(type) {
		case *FillReq:
			m.topPort.RetrieveIncoming()
			m.fill(msg)
			madeProgress = true
		case *LookupReq:
			if m.latency > 0 {
				if len(m.reads) >= m.maxRequestsInFlight {
					return madeProgress
				}
				m.topPort.RetrieveIncoming()
				m.reads = append(m.reads, readTransaction{
					req:       msg,
					cycleLeft: m.latency,
				})
				madeProgress = true
				continue
			}

			if !m.sendLookupRsp(msg) {
				return madeProgress
			}
			m.topPort.RetrieveIncoming()
			madeProgress = true
		default:
			log.Panicf("pagewalkcache cannot handle message of type %T", item)
		}
	}
	return madeProgress
}

func (m *middleware) sendLookupRsp(req *LookupReq) bool {
	if !m.topPort.CanSend() {
		return false
	}

	segment, validLevel := m.segment(req)
	hit := false
	if validLevel {
		set := &m.sets[req.Level]
		wayID, found := setLookup(set, req.PID, segment)
		hit = found
		if found {
			setVisit(set, wayID)
		}
	}

	rsp := &LookupRsp{
		MsgMeta: sim.MsgMeta{
			ID:           sim.GetIDGenerator().Generate(),
			Src:          m.topPort.AsRemote(),
			Dst:          req.Src,
			TrafficClass: "pagewalkcache.LookupRsp",
		},
		RspTo:   req.ID,
		Hit:     hit,
		PID:     req.PID,
		VAddr:   req.VAddr,
		Level:   req.Level,
		Segment: segment,
	}
	return m.topPort.Send(rsp) == nil
}

func (m *middleware) markedForRemoval(index int) bool {
	for _, marked := range m.removeReads {
		if marked == index {
			return true
		}
	}
	return false
}
