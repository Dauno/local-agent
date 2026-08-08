---
# Refactor Handoff — slack-local-agent

Estado del refactor de sobre-ingeniería/duplicación por batches. Este documento es el punto de continuidad entre sesiones.

## Estado actual

- 18 batches de refactor completados + docs update, 20 commits de refactor (A8 y A11 divididos en dos commits), neto acumulado **−277 líneas**. A12b aplicado: el hallazgo #14 de ponytail-review se aplicó al eliminar el espejo de resultado de OpenCode.
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

## Proceso por batch

1. Leer los archivos de producción del path.
2. Evaluar si conviene invocar `ponytail-review` (agente más capaz para over-engineering); si aplica, usar su salida como base de los hallazgos.
3. Presentar hallazgos con neto estimado de líneas + caveats.
4. Asignar el batch a Luna con aprobación explícita previa del operador.

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
- **Scope por delegación:** máximo 2–3 archivos por invocación de worker/review; nunca carpetas completas ni contexto de conversación entero. Dividir batches grandes en sub-batches.
- Si un batch supera 3 archivos de producción, partir en sub-batches antes de delegar.

## Pendientes

- Revisión de comportamiento en `internal/adapter/slack`: `canvas_creator.go` vs `generated_file_uploader.go` — mismo switch de clasificación de ambigüedad (SlackErrorResponse fatal/internal/service_unavailable, RateLimitedError, StatusCodeError ≥ 500) con defaults deliberadamente distintos (canvas: ambiguous=true por defecto; uploader: false + sniffing de texto). Requiere revisión normal de comportamiento, no dedup mecánica.
- Resto del árbol: `internal/domain`, `internal/port`, otros adapters (mismo proceso).
- Tracking en Slack Canvas (opcional).
- `RequestBudgetPolicy` y diagnósticos no métricos en domain: conservados en A1 porque eliminarlos requiere ampliar el alcance a consumidores externos; revisar en batch futuro si se amplía scope.
- Nota de comportamiento: `unicode.IsControl` en `SanitizeConversationSummary` (A2) también rechaza controles C1 que la condición anterior no rechazaba — comportamiento levemente más estricto, documentado.
- `memory_core.go`: cambios preexistentes sin stagear quedaron intactos; A3 se aplicó con stage selectivo.
- A4: el grep global no permitió eliminar `ContinuityItem.Kind` (sin lecturas directas, pero con construcciones en `adkagent` y tests), `ContinuityCapsule.Superseded` (uso en `adkagent`, persistencia SQLite y test), ni `SourceEventOrdinal`, `SourceSessionRevision`, `SourceDigest`, `SupersedesID` (escritura en `adkagent`, validación SQLite y tests). `AgentContext.MaxChars` también se conserva: Slack lo construye, `adkagent` lo lee como budget y hay referencia en test. No se eliminó ninguno de estos campos.
- A5: el grep global de los ítems 6-8 no permitió eliminar las formas largas de `MessageSource` (consumidores externos en producción e integración/tests), `WithInferredSource` (dos consumidores SQLite) ni `ValidateACPAllowlist` (dos llamadores en `agentbuilder`). Las reducciones mecánicas sí eliminaron ramas de validación duplicadas, la clonación innecesaria de mensajes y las construcciones repetidas del error de kind.
- A7: las nueve reducciones aprobadas en `internal/usecase/bot` se aplicaron tras grep global sin consumidores externos. Commit de refactor: `+34/-37`, neto **−3 líneas**; build y suite del paquete verdes; tests sin cambios.
- A8: C1, C2 y C4 aplicados en `877994b` (`+6/-15`, neto **−9**); C3 se conservó por referencias en tests y declaraciones de domain, y C5 por fakes de contadores inyectados y la cobertura vigente del guard byte-bound. S1 y S2 aplicados en `2712523` (`+12/-34`, neto **−22**); build y suites de ambos paquetes verdes; tests sin cambios. El bug latente de `responseCountBefore` stale en el segundo recount fue confirmado y queda fuera de este refactor.
- A9: `hasControl` y `destination` eliminados en `8336f08` (`+4/-19`, neto **−15**); se reutilizó `domain.ConversationReplyTarget` y se preservó el mensaje de destino inválido. Paquete generatedfile verde; tests sin cambios.
- A10: `activationErrorRetryable` y `currentActivation` inlineados, y `systemClock` sustituido por `port.SystemClock`, en `6e693c2` (`+4/-17`, neto **−13**); `noopLogger` se conservó porque no existe `port.NoopLogger`. Paquete externalagent verde tras repetir un timeout intermitente; tests sin cambios.
- A11: ponytail-review usado. En doctor se eliminaron el alias `summarizerCompatible`, el caso `transcriptionResolved == nil` inalcanzable y `Report.Passed()` sin callers; en opencode se reemplazó el loop de autorización por `slices.Contains` preservando deny-if-empty. El corte de parámetros duplicados de `Probe` se saltó porque `service_test.go` pasa valores explícitos distintos de `deps`; no se tocaron tests. Commits `4fada2b` (`+2/-5`, neto **−3**) y `731130a` (`+2/-6`, neto **−4**); build, vet, suite y `git diff --check` verdes.
- A12a: se reutilizaron los hallazgos de ponytail-review de A11; no se invocó una revisión nueva. A12a.1 extrajo el chequeo duplicado de token counter en `a8b4fbb` (`+18/-26`, neto **−8**), preservando mensajes, remediaciones y orden. A12a.2 se saltó: `validateTranscriptionModel` solo exige URL no vacía y usa mensajes distintos, mientras `validateAudioTranscriptionProfile` exige URL absoluta HTTP/HTTPS y rechaza credenciales o fragmentos; compartir helper cambiaría comportamiento. No se modificaron tests. A12b aplicó el hallazgo #14 de ponytail-review: `Status`, `Probe`, `Upgrade` y `Rollback` devuelven `domain.OpenCodeManagementResult`; se eliminaron `opencode.Result`, `resultFromManager` y la conversión 1:1 del tool, preservando mensajes. Commit `c2fd485` (`+33/-58`, neto **−25**); no se modificaron tests.
---
