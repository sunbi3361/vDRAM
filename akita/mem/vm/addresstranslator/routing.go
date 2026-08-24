package addresstranslator

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type translationRoute struct { // sbin_codex
	port             sim.Port
	mapper           mem.AddressToPortMapper
	address          uint64
	pid              vm.PID
	remoteDemandInfo *mem.RemoteDemandInfo
}

func (m *middleware) routeTranslation(req mem.AccessReq, page vm.Page) translationRoute { // sbin_codex
	offset := req.GetAddress() % (1 << m.log2PageSize)
	physicalAddress := page.PAddr + offset
	if page.RemoteAccessible && m.remoteBottomPort != nil {
		return translationRoute{
			port: m.remoteBottomPort, mapper: m.remoteMemoryPortMapper,
			address: physicalAddress,
			remoteDemandInfo: &mem.RemoteDemandInfo{
				PID: req.GetPID(), VAddr: req.GetAddress(), DeviceID: m.deviceID,
			},
		}
	}
	if m.virtualAddressForLocalMemory {
		return translationRoute{
			port: m.bottomPort, mapper: m.memoryPortMapper,
			address: req.GetAddress(), pid: req.GetPID(),
		}
	}
	return translationRoute{
		port: m.bottomPort, mapper: m.memoryPortMapper,
		address: physicalAddress,
	}
}

func (m *middleware) createTranslatedReq(req mem.AccessReq, route translationRoute) mem.AccessReq { // sbin_codex
	switch req := req.(type) {
	case *mem.ReadReq:
		return m.createTranslatedReadReq(req, route)
	case *mem.WriteReq:
		return m.createTranslatedWriteReq(req, route)
	default:
		log.Panicf("cannot translate request of type %s", reflect.TypeOf(req))
		return nil
	}
}

func (m *middleware) createTranslatedReadReq(req *mem.ReadReq, route translationRoute) *mem.ReadReq { // sbin_codex
	builder := mem.ReadReqBuilder{}.
		WithSrc(route.port.AsRemote()).
		WithDst(route.mapper.Find(route.address)).
		WithAddress(route.address).
		WithByteSize(req.AccessByteSize).
		WithPID(route.pid).
		WithInfo(req.Info)
	if route.remoteDemandInfo != nil {
		builder = builder.WithRemoteDemandInfo(*route.remoteDemandInfo)
	}
	clone := builder.Build()
	clone.CanWaitForCoalesce = req.CanWaitForCoalesce
	return clone
}

func (m *middleware) createTranslatedWriteReq(req *mem.WriteReq, route translationRoute) *mem.WriteReq { // sbin_codex
	builder := mem.WriteReqBuilder{}.
		WithSrc(route.port.AsRemote()).
		WithDst(route.mapper.Find(route.address)).
		WithData(req.Data).
		WithDirtyMask(req.DirtyMask).
		WithAddress(route.address).
		WithPID(route.pid).
		WithInfo(req.Info)
	if route.remoteDemandInfo != nil {
		builder = builder.WithRemoteDemandInfo(*route.remoteDemandInfo)
	}
	clone := builder.Build()
	clone.CanWaitForCoalesce = req.CanWaitForCoalesce
	return clone
}
