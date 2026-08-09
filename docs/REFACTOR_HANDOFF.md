---
# Refactor Handoff — slack-local-agent

Estado del refactor de sobre-ingeniería/duplicación por batches. Este documento es el punto de continuidad entre sesiones.

## Estado actual

- 21 batches de refactor completados + docs update, 24 commits de refactor (A8, A11 y A13a-ext divididos en dos commits), neto acumulado **−359 líneas**. A13a-ext aplicó los cortes 3, 9 y 10; el corte 8 se saltó por falta de contrato de directorios padre en `ProjectFiles.CreateFile`. A13c2 aplicó los cortes 2, 3 y 4; saltó 1 y 5 por la regla de no tocar tests.
- Tests verdes en todo momento; los commits del refactor no modificaron archivos de test.
- Los cambios preexistentes sin stagear quedan fuera de estos commits.

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
| A13c1.1 (cortes 1, 2, 5, 6, 7, 8 y 9) | `internal/usecase/sandbox/service.go` | `1aa7e5e` (`+8/-56`, neto **−48**) |
| A13c2.1 (cortes 2, 3 y 4) | `internal/adapter/memorycurator/curator.go` | `53886f3` (`+6/-22`, neto **−16**) |

## Proceso y reglas por batch

- Leer producción; decidir y anotar si se usa `ponytail-review`; presentar hallazgos en tabla con neto y caveats.
- Ejecutar solo tras aprobación explícita; Luna usa job durable y máximo 2–3 archivos por invocación.
- No modificar tests; preservar mensajes de error/log exactos; revisar `git status` antes y dejar cambios preexistentes fuera.
- Crear un commit conventional por batch y verificar `go build ./...`, `go vet ./...`, `go test` (paquete y suite) y `git diff --check`.
- Cada commit debe tener neto < 0; revertirlo si no cumple. No invocar workers sobre carpetas completas ni sobre todo el contexto.

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
- A13c1: `ponytail-review` usado. Aplicados los cortes 1 (gate: `ErrUnauthorized` solo aparecía en `service.go`), 2 (gates: sin inyección `Clock:` en producción y sin uso de `Clock` en tests), 5 (gate: sin lectores de `SandboxResult.Error`), 6 (gate: `isAllowed` solo tenía su declaración y una llamada), 7 (ambos `validateRelativePath` pass-through conservan el mensaje byte-idéntico), 8 (gate: `createWorktreeTool` no tiene registro real y no existe executor para `CapRunCommand`) y 9 (gate: un helper y una llamada; inline con comportamiento byte-idéntico). Se saltaron 3 (gate fallido: `internal/adapter/toolfactory/toolfactory_test.go:602` lee `op.Actor`) y 4 (gate fallido: `internal/usecase/sandbox/service_test.go:205` y `internal/adapter/fssandbox/sandbox_test.go:74,109` leen `OutputBytes`). Commit `1aa7e5e` (`+8/-56`, neto **−48**); acumulado **−343**. No se modificaron tests.
- A13c2: `ponytail-review` usado. Registro completo de los cinco hallazgos, con localización original:

| Hallazgo | Localización original | Veredicto y motivo |
|---|---|---|
| #1 extracción/merge trusted-entity | `curator.go:82,86-93,149-165`; test `curator_test.go:103-126` | **SALTADO**: requiere borrar o editar tests; la regla de no tocar tests lo excluye. |
| #2 recuperación de substring JSON (`extractJSONObject`) | `curator.go:242,281-290` | **APLICADO**: gate aprobado; no había referencias en tests. Se dejó `strings.TrimSpace(response)`. |
| #3 `ModelCalls` duplicado | `curator.go:31,50,69` | **APLICADO**: gate aprobado; no había referencias al campo en tests. Se usa `c.config.ModelCalls`. |
| #4 branch de timeout negativo | `curator.go:64-68` | **APLICADO**: gate aprobado; no había configuraciones `Timeout` negativas ni dependencia del branch en tests. |
| #5 receiver de `parsePatch` y `emptyPatchLLM` | `curator.go:241`; tests `curator_test.go:129-133,135,140,149-154` | **SALTADO**: requiere tocar call sites y fake de tests; la regla de no tocar tests lo excluye. |

- A13c2 commit de producción: `53886f3`, `git show --shortstat`: `+6/-22`, neto **−16**. El segundo commit de este batch es esta actualización documental; su hash y shortstat se registran en el reporte de cierre.
---
