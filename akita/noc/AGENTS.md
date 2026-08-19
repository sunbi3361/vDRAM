# PROJECT KNOWLEDGE BASE
# branch: main | package: akita/noc

## OVERVIEW

`akita/noc` is the network-on-chip layer. It moves `sim.Msg` traffic between Akita components by splitting messages into flits, switching them through a pipeline, and reassembling them at the destination.

## STRUCTURE

- `messaging/` - flits (`Flit`, `FlitBuilder`) and traffic counters.
- `networking/switching/` - endpoint fragmentation/reassembly and switch pipeline.
- `networking/routing/` - per-switch routing tables.
- `networking/arbitration/` - crossbar arbiter for output ports.
- `networking/networkconnector/` - generic topology builder and routers.
- `networking/mesh/` - 3D mesh/torus connector.
- `networking/pcie/` - PCIe tree connector.
- `networking/nvlink/` - PCIe, NVLink, and ethernet hybrid connector.
- `wiring/` - pull-based zero-latency wire between local ports.
- `standalone/` - simple traffic agents for isolated NoC tests.
- `acceptance/` - end-to-end Go scenarios driven by `acceptance_test.py`.

## WHERE TO LOOK

- `messaging/flit.go` - flit carries `SeqID`, `NumFlitInMsg`, the parent `Msg`, and the switch `OutputBuf`.
- `networking/switching/endpoint/endpoint.go` - fragments a message into flits, sends them out, receives flits, and reassembles the original message before delivery.
- `networking/switching/switches/switch.go` - five-stage tick: send out, forward via arbiter, route to output buffer, advance pipeline, start processing incoming flits.
- `networking/networkconnector/connector.go` - adds switches, connects devices and switches, and establishes routes.
- `networking/mesh/mesh.go`, `pcie/pcie.go`, `nvlink/connector.go` - topology-specific connectors built on `networkconnector`.
- `wiring/wire.go` and `wiring/port.go` - single-slot pull wire.
- `standalone/agent.go` and `trafficinjector.go` - reusable test agents.

## CONVENTIONS

- Endpoints own fragmentation. A message becomes one or more flits based on `TrafficBytes`, `flitByteSize`, and `encodingOverhead`.
- Reassembly waits until `numFlitArrived == numFlitRequired` for a message ID, then delivers the original message to the matching device port.
- Each switch port has its own pipeline plus route, forward, and send-out buffers.
- Routing tables map a final destination port to a next-hop local port. The default route is used when no exact match exists.
- The crossbar arbiter selects one requesting input buffer per output buffer each cycle, rotating the starting port for fairness.

## ANTI-PATTERNS

- Do not set `LinkParameter.IsIdeal = false`. Non-ideal links currently panic in `networkconnector.connectPorts`.
- Do not rely on `wiring.Wire` for multi-hop networks. It is pull-based, has no buffering, and forbids same-cycle peek/retrieve of a message that was just sent.
- Do not plug a non-`wiring.Port` into a `wiring.Wire`. It panics.
- Do not assume NVLink ethernet links work. `ConnectSwitchesWithEthernetLink` sets `IsIdeal = false`, so it hits the same panic.
- Do not forget to call `EstablishRoute` after building a topology, or switches will have empty routing tables.

## TESTING

- Unit tests live next to each package and use Ginkgo.
- Run the NoC acceptance suite with `python3 acceptance/acceptance_test.py` from the `akita` root.
- Acceptance scenarios cover one-to-one, single switch, three agents on one switch, PCIe point-to-point/random, DGX-style NVLink, and mesh traffic.
