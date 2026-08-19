# akita/sim KNOWLEDGE BASE

Inherits `/home/sbin/vdram_v3/AGENTS.md` and `/home/sbin/vdram_v3/akita/AGENTS.md`.

## OVERVIEW

akita/sim is the discrete-event simulation kernel. It provides the time base (VTimeInSec), event scheduling, two engine implementations, component/port wiring, and messaging primitives used by every higher-level Akita package.

## WHERE TO LOOK

- engine.go, serialengine.go, parallelengine.go: Engine, SerialEngine, ParallelEngine.
- event.go: Event, EventBase, Handler.
- component.go: Component, ComponentBase.
- port.go, portowner.go: Port, PortOwner, PortOwnerBase.
- ticker.go: Ticker, TickScheduler, TickingComponent.
- middleware.go: Middleware, MiddlewareHolder.
- hook.go: Hookable, HookableBase.
- buffer.go: Buffer.
- idgenerator.go: IDGenerator and sequential/parallel generators.
- directconnection/: zero-latency connection example using MiddlewareHolder.

## CONVENTIONS

- Names passed to NewComponentBase, NewBuffer, NewPort, and NewTickingComponent must satisfy NameMustBeValid: hierarchical CamelCase tokens separated by dots, no empty tokens, no underscores/hyphens/quotes.
- Events are immutable after scheduling; handlers must not mutate the Event.
- Handlers operate only on their own domain state; cross-component changes happen through Ports.
- A component's Handle method must be safe to invoke by the Engine.
- Tick() returns true to request another tick, false when idle.
- Use NewTickingComponent for primary ticks and NewSecondaryTickingComponent for deferred same-time ticks.
- Port Send validates that the port is the message source, the destination is non-empty, and source != destination.

## ANTI-PATTERNS

- Do not schedule events earlier than Engine.CurrentTime(); both engines panic.
- Do not swap ID generators after the first Generate call; the package panics.
- Do not rely on parallel ID generators for deterministic results; UseSequentialIDGenerator for reproducible simulations.
- Do not bypass Port to mutate another component's state.
- Do not assume Port buffer capacity is unbounded; check CanSend and handle SendError.
- Do not call Unplug on directconnection.Comp; it panics.

## TESTING

- Unit tests use Ginkgo/Gomega in *_test.go files alongside the source.
- Engine behavior is covered in serialengine_test.go and parallelengine_test.go.
- Component/port/buffer/ticker tests are in component_test.go, port_test.go, buffer_test.go, and ticker_test.go.
- directconnection tests demonstrate connection-level integration.
- Run with `cd akita && ginkgo -r` or `go test ./sim/...`.
