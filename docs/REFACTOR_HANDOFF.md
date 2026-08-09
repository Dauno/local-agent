---
# Refactor Handoff — slack-local-agent

Estado del refactor de sobre-ingeniería/duplicación por batches. Este documento es el punto de continuidad entre sesiones.

## Estado actual

- 19 batches de refactor completados + docs update, 22 commits de refactor (A8, A11 y A13a-ext divididos en dos commits), neto acumulado **−295 líneas**. A13a-ext aplicó los cortes 3, 9 y 10; el corte 8 se saltó por falta de contrato de directorios padre en `ProjectFiles.CreateFile`.
- Tests verdes en todo momento; no se modificó ningún archivo de test.
- 3 archivos con cambios preexistentes sin commitear (no tocar, quedan fuera de cualquier commit).

## Commits por batch

| Batch | Alcance | Commit |
|---|---|---|
| agentdef | 7/7 archivos | `67fdb38` |
| bootstrap + agentbuilder | — | `19890b3` |
| bot | 6/6 archivos | `fd0a631` |
| memory | 3/3 archivos | `764487b` |
| canvas | 1/1 archivo | `87f3dab` |
| cross-cutting | systemClock → port.SystemClock (canvas, memory/runner, generatedfile) | `4e306aa` |
| A1 | `internal/domain/context_budget.go` | `1d1a5d7` |
| A2 | `internal/domain/compaction.go` + `context_metrics.go` | `3a11a1d` |
| A3 | `internal/domain/memory_core.go` + `memory_entity.go` + `memory_instruction.go` | `4476419` |
| A4 | `internal/domain/continuity.go` + `invocation.go` | `89a498a` |
| A5 | `internal/domain/message.go` + `validation.go` | `aed5d0e` |
| A7 | `internal/usecase/bot` (`service.go`, `job_completion.go`, `streaming.go`) | `c0ad0a0` |
| A8.1 | `internal/usecase/contextcompiler/compiler.go` | `877994b` (net `−9`) |
| A8.2 | `internal/usecase/contextsummary/service.go` | `2712523` (net `−22`) |
| A9 | `internal/usecase/generatedfile/service.go` | `8336f08` (`+4/-19`, net `−15`) |
| A10 | `internal/usecase/externalagent/activations.go` | `6e693c2` (`+4/-17`, net `−13`) |
| A11.1 | `internal/usecase/doctor/service.go` | `4fada2b` (`+2/-5`, net `−3`) |
| A11.2 | `internal/usecase/opencode/service.go` | `731130a` (`+2/-6`, net `−4`) |
| A12a.1 | `internal/usecase/doctor/service.go` | `a8b4fbb` (`+18/-26`, net `−8`) |
| A12a.2 | `internal/app/cli_model.go` + `internal/usecase/doctor/service.go` | **saltado**: validaciones, mensajes y casos límite no son equivalentes |
| A12b | `internal/usecase/opencode/service.go` + `internal/app/opencode_tools.go` | `c2fd485` (`+33/-58`, net `−25`) |
| A13a-ext.1 (corte 3) | `internal/usecase/agentbuilder/service.go` | `43d2503` (`+0/-8`, net `−8`) |
| A13a-ext.2 (cortes 9/10) | `internal/usecase/bootstrap/service.go` + `internal/app/application.go` | `501cd5e` (`+5/-15`, net `−10`) |
| A13b1.2 (corte 2) | `internal/port/agentbuilder.go` + `internal/usecase/agentbuilder/service.go` | **SALTADO (no aplicado)**: `TestDependencyDirection`; `internal/port` no puede depender de `agentdef` y `current any` es desacoplamiento deliberado de la interfaz. Propuesto en `f61abc2`, revertido en `2e61b22`; acumulado se mantiene en **−295** (neto 0) |

## Proceso por batch

1. Leer los archivos de producción del path.
2. Evaluar si conviene invocar `ponytail-review` (agente más capaz para over-engineering); si aplica, usar su salida como base de los hallazgos.
3. Presentar hallazgos con neto estimado de líneas + caveats; asignar el batch a Luna con aprobación explícita previa del operador.

## Reglas por batch

- Para cada batch: decidir explícitamente **usar o no `ponytail-review`** y anotar la decisión.
- **Presentar hallazgos en tabla** (archivo / hallazgo / neto aprox.) con caveats.
- **Luna ejecuta el batch** en job durable — 1 commit por batch, aprobación explícita antes de invocar.

## Reglas del refactor

- No modificar archivos de test.
- Preservar mensajes de error/log exactos (los tests los fijan).
- `git status` antes de tocar; cambios preexistentes fuera del commit.
- 1 commit por batch, mensaje conventional (refactor/docs/...).
- Verificación: `go build ./...`, `go vet ./...`, `go test` (paquete + suite), `git diff --check`.
- Refactors con neto ≤ 0 se descartan (ej.: clasificador de ambigüedad compartido, +3 neto).
- **Scope por delegación:** máximo 2–3 archivos por invocación de worker/review; nunca carpetas completas ni contexto de conversación entero; partir batches mayores en sub-batches.

## Pendientes

- Revisión pendiente: clasificación de ambigüedad en `internal/adapter/slack` (`canvas_creator.go` vs `generated_file_uploader.go`), resto de `internal/domain`, `internal/port` y adapters, y tracking opcional en Slack Canvas.
- `RequestBudgetPolicy`, diagnósticos no métricos y el comportamiento más estricto de `unicode.IsControl` en A2 se conservan; revisar solo con alcance ampliado.
- `memory_core.go`: cambios preexistentes sin stagear quedaron intactos; A3 se aplicó con stage selectivo.
- A4: el grep global no permitió eliminar `ContinuityItem.Kind` (sin lecturas directas, pero con construcciones en `adkagent` y tests), `ContinuityCapsule.Superseded` (uso en `adkagent`, persistencia SQLite y test), ni `SourceEventOrdinal`, `SourceSessionRevision`, `SourceDigest`, `SupersedesID` (escritura en `adkagent`, validación SQLite y tests). `AgentContext.MaxChars` también se conserva: Slack lo construye, `adkagent` lo lee como budget y hay referencia en test. No se eliminó ninguno de estos campos.
- A5: el grep global de los ítems 6-8 no permitió eliminar las formas largas de `MessageSource` (consumidores externos en producción e integración/tests), `WithInferredSource` (dos consumidores SQLite) ni `ValidateACPAllowlist` (dos llamadores en `agentbuilder`). Las reducciones mecánicas sí eliminaron ramas de validación duplicadas, la clonación innecesaria de mensajes y las construcciones repetidas del error de kind.
- A7: las nueve reducciones aprobadas en `internal/usecase/bot` se aplicaron tras grep global sin consumidores externos. Commit de refactor: `+34/-37`, neto **−3 líneas**; build y suite del paquete verdes; tests sin cambios.
- A8: C1, C2 y C4 aplicados en `877994b` (`+6/-15`, neto **−9**); C3 se conservó por referencias en tests y declaraciones de domain, y C5 por fakes de contadores inyectados y la cobertura vigente del guard byte-bound. S1 y S2 aplicados en `2712523` (`+12/-34`, neto **−22**); build y suites de ambos paquetes verdes; tests sin cambios. El bug latente de `responseCountBefore` stale en el segundo recount fue confirmado y queda fuera de este refactor.
- A9: `hasControl` y `destination` eliminados en `8336f08` (`+4/-19`, neto **−15**); se reutilizó `domain.ConversationReplyTarget` y se preservó el mensaje de destino inválido. Paquete generatedfile verde; tests sin cambios.
- A10: `activationErrorRetryable` y `currentActivation` inlineados, y `systemClock` sustituido por `port.SystemClock`, en `6e693c2` (`+4/-17`, neto **−13**); `noopLogger` se conservó porque no existe `port.NoopLogger`. Paquete externalagent verde tras repetir un timeout intermitente; tests sin cambios.
- A11: ponytail-review usado. En doctor se eliminaron el alias `summarizerCompatible`, el caso `transcriptionResolved == nil` inalcanzable y `Report.Passed()` sin callers; en opencode se reemplazó el loop de autorización por `slices.Contains` preservando deny-if-empty. El corte de parámetros duplicados de `Probe` se saltó porque `service_test.go` pasa valores explícitos distintos de `deps`; no se tocaron tests. Commits `4fada2b` (`+2/-5`, neto **−3**) y `731130a` (`+2/-6`, neto **−4**); build, vet, suite y `git diff --check` verdes.
- A12a: se reutilizaron los hallazgos de ponytail-review de A11; no se invocó una revisión nueva. A12a.1 extrajo el chequeo duplicado de token counter en `a8b4fbb` (`+18/-26`, neto **−8**), preservando mensajes, remediaciones y orden. A12a.2 se saltó: `validateTranscriptionModel` solo exige URL no vacía y usa mensajes distintos, mientras `validateAudioTranscriptionProfile` exige URL absoluta HTTP/HTTPS y rechaza credenciales o fragmentos; compartir helper cambiaría comportamiento. No se modificaron tests. A12b aplicó el hallazgo #14 de ponytail-review: `Status`, `Probe`, `Upgrade` y `Rollback` devuelven `domain.OpenCodeManagementResult`; se eliminaron `opencode.Result`, `resultFromManager` y la conversión 1:1 del tool, preservando mensajes. Commit `c2fd485` (`+33/-58`, neto **−25**); no se modificaron tests.
- A13a-ext: `ponytail-review` usado en el segundo intento, exitoso. Corte 3 aplicado en `43d2503`; no hubo callers/tests de `Preview` dependientes del alias `Model`. Cortes 9/10 aplicados en `501cd5e`; `CanonicalRoot` se resolvió una vez en `app.New`, y se preservaron mensajes y firmas. Corte 8 **saltado**: aunque el adapter crea padres, `ProjectFiles.CreateFile` no lo promete en su contrato y bootstrap no debe depender de ese detalle concreto. No se modificaron tests.
- A13b1: `ponytail-review` hallazgo #2 inválido por arquitectura. El corte 2 se marcó **SALTADO (no aplicado)** porque `TestDependencyDirection` prohíbe que `internal/port` dependa de `agentdef`; `current any` es desacoplamiento deliberado de la interfaz. La propuesta `f61abc2` fue revertida en `2e61b22`; acumulado **−295** (neto 0 del revert). No se modificaron tests.
---
