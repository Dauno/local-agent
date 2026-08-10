---
# Refactor Handoff — slack-local-agent

Estado del refactor de sobre-ingeniería/duplicación por batches. Este documento es el punto de continuidad entre sesiones.

## Estado actual

- El acumulado de refactor hasta A13c8 era **−797 líneas**. La cadena A13d (001 → 003 → 002 → 004 → 005) está completada en `refactor/context-compiler-improvements` con cinco commits, sin merge a `main`; el acumulado contabilizado de la cadena es **+756** y el acumulado global queda en **−41** (`−797 + 756`).
- Gates en verde: `go test ./...`, `go vet ./...` y `go build -trimpath ./cmd/local-agent`. Queda pendiente el merge a `main` con el visto bueno final del usuario.
- Los cambios preexistentes de `memory_core.go`, `redact.go` y `memory/service.go` se incorporaron en el Commit 1 de A13c3 por decisión explícita del usuario. El diff real correspondía a `internal/domain/memory_core.go`, `internal/secure/redact.go` e `internal/usecase/memory/service.go`; no existían los dos primeros bajo `internal/usecase/memory`.

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
| A13c3.0 | cambios preexistentes de memory core, redaction y validación de operaciones | `4ea1941` (`+12/-31`, neto **−19**) (excluido del acumulado de refactor) |
| A13c3.1 (cortes 1, 2, 3, 5, 6, 7 y 8) | `internal/usecase/memory/service.go` + `runner.go` | `4883914` (`+50/-88`, neto **−38**) |
| A13c3.2 | actualización de este handoff | este commit; hash y shortstat se reportan en el cierre |
| A13c7.1 | `internal/domain`: elimina `ValidACPProgressHealth`, `ACPToolProgress.Validate` y `newProtocolLedger`; inlinea el retorno de `ValidArtifactResult` | `71d6865` (`+2/-36`, neto −34) |
| A13c7.2 | `internal/adapter/toolfactory`: elimina las herramientas `create_worktree` y `remove_worktree`, elimina `isAllowedUser` y usa helpers stdlib para claves/autorización | `afad535` (`+6/-66`, neto −60) |
| A13c7.3 | `internal/adapter/toolrunner`: elimina `Executor.Tools`; usa `slices.Contains` y `maps.Keys`/`slices.Sorted` | `0504525` (`+4/-24`, neto −20) |
| A13c7.4 | `internal/adapter/openaillm`: elimina métricas de stream y `ConfigureMetrics`; usa `maps.Keys`/`slices.Sorted` para headers | `5cb2d2c` (`+3/-39`, neto −36) |
| A13c7.5 | `internal/adapter/adkagent`: elimina `CompactionMetrics` y sus snapshots/contadores; elimina helper local `min` | `c25f472` (`+1/-51`, neto −50) |
| A13c7.6 | C2: `port.SystemClock` en lugar de 2 duplicaciones locales (`bot` + `externalagent`) | `8269644` (`+3/-11`, neto −8) |
| A13c7.7 | D8 (bounds muertos + parámetros artifact store), S2 (builtin min), S3a (`context.WithTimeout`), S6 (concatenación directa) en `acpclient` + `app/composition` | `3105eee` (`+11/-38`, neto −27) |
| A13c7.8 | C3: `ambiguousSlackError` compartido entre `canvas_creator.go` y `generated_file_uploader.go` | `7d79e63` (`+14/-19`, neto −5) |
| A13c7.9 | S3b (`withTimeout`→`context.WithTimeout`) + S4 (`containsString`→`slices.Contains`) en manager+shim | `cb615ce` (`+4/-19`, neto −15) |
| A13c7.10 | S9 (`minInt64`/`maxInt64`→builtin min/max; `maxInt()`→`math.MaxInt`) + S10 (2 locales `^uint(0)>>1`→`math.MaxInt`) en stores+tokencounter | `c44a15c7` (`+10/-28`, neto −18) |
| A13c7.11 | D9 (maps muertos `topicByID` + params muertos en `writeRootIndex`/`writeNestedIndex`) + D11 (campo kind redundante en key + `metricKey` sin param kind) en okf+recorder | `53fc486d` (`+8/-19`, neto −11) |
| A13c7.12 | C1: `domain.FlattenTurns` (inlina `Content.Clone()`) + eliminadas 2 copias locales `flattenTurns` + 5 call sites | `73ce7fde` (`+17/-21`, neto −4) |
| A13d.001 | Fail-Closed Compiler Contract: validaciones fail-closed en `contextcompiler` | `0a776a2e` (`+177/-3`, neto +174) |
| A13d.003 | Remove Unused Session Snapshot: elimina `SessionRevision`/snapshot sin uso | `5cba0147` (`+86/-43`, neto +43) |
| A13d.002 | Trusted Projection Materialization (P0): proyecciones solo desde resultados almacenados; `compiler.go`, `compiler_test.go`, `adversarial_test.go` | `d7908f4` (`+635/-159`, neto +476) |
| A13d.004 | Compiler Phase Structure (P2): `Compile` orquesta fases; `analysis.go`/`allocation.go`/`externalization.go` | `8c2e6109` (`+1392/-934`, neto +458) |
| A13d.005 | Cleanup Post-Review: aplica 8 hallazgos de ponytail-review; `maps.Clone`, `slices.Reverse`, elimina `TestCompilerPhaseOrder` | `b7c0e81` (`+105/-283`, neto −178) |

## Proceso y reglas por batch

- Leer producción; decidir y anotar si se usa `ponytail-review`; presentar hallazgos en tabla con neto y caveats.
- Ejecutar solo tras aprobación explícita; Luna usa job durable y máximo 2–3 archivos por invocación.
- No modificar tests; preservar mensajes de error/log exactos; revisar `git status` antes y dejar cambios preexistentes fuera, salvo decisión explícita del usuario como en A13c3.
- Crear un commit conventional por batch y verificar `go build ./...`, `go vet ./...`, `go test` (paquete y suite) y `git diff --check`.
- Cada commit de refactor debe tener neto < 0; revertirlo si no cumple. No invocar workers sobre carpetas completas ni sobre todo el contexto.

## Pendientes

- La cadena A13d está completa; queda pendiente el merge a `main`, sujeto a autorización y visto bueno final del usuario.
- Revisión pendiente: clasificación de ambigüedad en `internal/adapter/slack` (`canvas_creator.go` vs `generated_file_uploader.go`), resto de `internal/domain`, `internal/port` y adapters, y tracking opcional en Slack Canvas.
- `RequestBudgetPolicy`, diagnósticos no métricos y el comportamiento más estricto de `unicode.IsControl` en A2 se conservan; revisar solo con alcance ampliado.
- `memory_core.go`: A3 se aplicó con stage selectivo. Los cambios preexistentes posteriores de `internal/domain/memory_core.go`, `internal/secure/redact.go` e `internal/usecase/memory/service.go` se incorporaron por decisión del usuario en `4ea1941`.
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

- A13c3: `ponytail-review` usado. Registro completo de los doce hallazgos, con localización original y decisión:

| Hallazgo | Localización original | Veredicto y motivo |
|---|---|---|
| #1 outcomes de Recall | `internal/usecase/memory/service.go:29-31,61-64,128-157` | **APLICADO** en `4883914`: el gate mostró referencias solo en `service.go` y no hubo referencias en tests; el cuerpo quedó en `Recall` y devuelve snippets + error. |
| #2 wrapper `Validate` | `internal/usecase/memory/service.go:284-288` | **APLICADO** en `4883914`: el gate mostró solo el wrapper y la llamada de `runner.go`, sin uso en tests; `runner.go` llama `validatePatch` directamente. |
| #3 callback de `validateReferenceFields` | `internal/usecase/memory/service.go:293-312,329-336` | **APLICADO** en `4883914`: el gate mostró una declaración y dos llamadas, sin uso en tests; el helper recibe prefix y llama `add` directamente. |
| #4 switch de operation type | `internal/usecase/memory/service.go:318-324` | **SALTADO**: era parte del trabajo preexistente ya presente antes de A13c3 y quedó en `4ea1941`; no se volvió a tocar en el commit de cortes. |
| #5 goroutine de supervisión | `internal/usecase/memory/runner.go:75-85` | **APLICADO** en `4883914`: el gate encontró solo `done`, `close(done)` y `<-done` en este bloque; la recuperación quedó inline. |
| #6 cancelación y timer | `internal/usecase/memory/runner.go:87-100` | **APLICADO** en `4883914`: `NewTimer`, `timer.Stop` y `timer.C` solo aparecían en este bloque; se dejó un `select` con `time.After`. |
| #7 wrappers de retry | `internal/usecase/memory/runner.go:141-225` | **APLICADO** en `4883914`: el gate mostró siete llamadas y dos declaraciones, sin uso en tests; logger y `retryOutbox` quedaron directos en cada sitio. |
| #8 wrapper `rescheduleOutbox` | `internal/usecase/memory/runner.go:167,236-238` | **APLICADO** en `4883914`: el gate mostró dos referencias; `RescheduleOutboxItem` quedó inlineado. |
| #9 estado muerto de `coverageStore` | `internal/usecase/memory/outbox_coverage_test.go:146,212-216,279-281` | **SALTADO**: corte en tests; la regla de no tocar tests lo prohíbe. |
| #10 wrapper `newCoverageStore` | `internal/usecase/memory/outbox_coverage_test.go:60-63,222-229` | **SALTADO**: corte en tests; la regla de no tocar tests lo prohíbe. |
| #11 helper `itemKey` | `internal/usecase/memory/outbox_coverage_test.go:105,321-323` | **SALTADO**: corte en tests; la regla de no tocar tests lo prohíbe. |
| #12 retorno `key` no usado | `internal/usecase/memory/runner_test.go:25,60` | **SALTADO**: corte en tests; la regla de no tocar tests lo prohíbe. |

- A13c3 Commit 1: `4ea1941`, `git show --shortstat`: `+12/-31`, neto **−19**; contiene solo los tres archivos reales del diff preexistente y queda fuera del acumulado de refactor por decisión del usuario.
- A13c3 Commit 2: `4883914`, `git show --shortstat`: `+50/-88`, neto **−38**; no modificó tests. El acumulado de refactor queda en **−397** (`−359 − 38`).
- A13c3 Commit 3: esta actualización documental. Su hash y shortstat se reportan en el cierre; no se puede insertar su propio hash en el contenido sin crear un cuarto commit.

## A13c4

- Se revisó `internal/port` de forma propia en tres lotes: interfaces sospechosas, `external_agent_job.go` y el resto del directorio. Se revisaron aproximadamente 100 interfaces; no se encontraron interfaces muertas. Todas tienen un implementador `var _` y callers.
- No se usó `ponytail`: la revisión no necesitó una métrica de densidad de dead-weight.
- A13c4 no tiene commits propios; esta nota se incluye en el registro de A13c5 por decisión del usuario.

## A13c5

### Cortes aplicados

Los ocho gates repo-wide con `rg '\bSIMBOLO\b'` mostraron solo la definición y su comentario cuando existía. Se eliminaron estos símbolos:

| Símbolo | Archivo | Localización original |
|---|---|---|
| `NewDispatcher` | `internal/adapter/slack/dispatcher.go` | líneas 93-95 |
| `NewBuilderLauncherPublisher` | `internal/adapter/slack/builder_launcher.go` | línea 27 |
| `NewBuilderModalPresenter` | `internal/adapter/slack/builder_modal.go` | líneas 23-29 |
| `MustLoadTemplateCatalog` | `internal/adapter/slack/template_catalog.go` | líneas 87-89 |
| `RenderModal` | `internal/adapter/slack/template_renderer.go` | líneas 103-105 |
| `RenderMessage` | `internal/adapter/slack/template_renderer.go` | líneas 125-127 |
| `MessageFallback` | `internal/adapter/slack/template_renderer.go` | líneas 147-149 |
| `BuildView` | `internal/adapter/slack/builder_modal.go` | líneas 61-64 |

### Cortes saltados

Estos cuatro símbolos se conservaron porque sus callers directos verificados están solo en tests; no se modificaron tests:

| Símbolo | Motivo |
|---|---|
| `NewJobNotificationPublisher` | Callers en `internal/adapter/slack/job_notification_test.go` solamente. |
| `NewContextEnricher` | Callers directos en `internal/adapter/slack/context_test.go` solamente; `NewContextEnricherFromSDK` también lo usa internamente. |
| `BuildViewForCallback` | Callers en `internal/adapter/slack/builder_modal_test.go` y `internal/adapter/slack/builder_continuity_test.go` solamente. |
| `BuildViewForKind` | Callers en `internal/adapter/slack/builder_modal_test.go` solamente. |

### Evaluados y conservados

- `boundedTextBuilder` y sus helpers se usan en producción en `internal/adapter/slack/history.go`.
- `RenderMarkdownParts` tiene caller de producción en `internal/app/external_agent_jobs.go`.
- `ResolveProgressLabels` tiene caller de producción en `internal/app/composition.go`.
- Los métodos `WithResponsePublisher`, `WithAllowedUserIDs`, `WithBuilderPresenter`, `WithBuilderHandler`, `WithDispatcher` y `SetInteractiveHandler` de `Listener` tienen callers de producción en `internal/app/composition.go`.

### Commits y acumulado

- Commit 1 de cortes: `ef03454`, `git show --shortstat`: `+0/-54`, neto **−54**. Solo contiene los cinco archivos de producción de `internal/adapter/slack`; no modifica tests.
- Commit 2 de documentación: este commit; `git show --shortstat` será `+47/-0`, neto **+47**. Su hash real se reporta en el cierre; no se puede insertar su propio hash en el contenido sin crear un tercer commit.
- Acumulado de refactor tras A13c5: **−451** (`−397 − 54`).

## A13c6

### Cortes aplicados

Revisión propia; no se invocó `ponytail`. Se aplicaron los cortes #1 y #3 sin modificar tests:

| Hallazgo | Localización original | Veredicto y motivo |
|---|---|---|
| #1 `requiredSecrets` | `internal/app/model_builder.go:160-171` | **APLICADO**: no tenía callers repo-wide. Su validación de API key ya está en `newModelForResolved`; la validación de Slack permanece en `requiredSlackTokens` durante la preparación del runtime. |
| #3 `eligibleAgentNames` | `internal/app/agent_tools.go:88-101`; `internal/usecase/doctor/service.go:183-191` | **APLICADO**: las dos copias privadas byte-idénticas se eliminaron y ambos callers usan `agentdef.EligibleAgentNames`. La implementación pública de `agentdef` quedó intacta. |

### Corte saltado

| Hallazgo | Motivo |
|---|---|
| #2 `CancelForConversation` (`internal/usecase/externalagent/service.go:563-568`) | **SALTADO**: callers solo en tests (`authorization_test.go:31`), no pertenece a una interfaz de `internal/port`; la regla de no modificar tests lo excluye. |

### Commits y acumulado

- Commit de cortes: `da2ce75`, `git show --shortstat`: `+2/-53`, neto **−51**.
- Commit de documentación: este commit; su hash real se reporta en el cierre. No se puede insertar su propio hash en el contenido sin crear un tercer commit.
- Acumulado de refactor tras A13c6: **−502** (`−451 − 51`). El commit documental no suma al acumulado.
---

## A13c7

- Revisión propia; no se invocó ponytail-review; hallazgos verificados con evidencia del repo antes de cada commit.

### Commits y acumulado

| Commit | Shortstat | Neto |
|---|---|---|
| `71d6865` | `+2/-36` | −34 |
| `afad535` | `+6/-66` | −60 |
| `0504525` | `+4/-24` | −20 |
| `5cb2d2c` | `+3/-39` | −36 |
| `c25f472` | `+1/-51` | −50 |
| `8269644` | `+3/-11` | −8 |
| `3105eee` | `+11/-38` | −27 |
| `7d79e63` | `+14/-19` | −5 |
| `cb615ce` | `+4/-19` | −15 |
| `c44a15c7` | `+10/-28` | −18 |
| `53fc486d` | `+8/-19` | −11 |
| `73ce7fde` | `+17/-21` | −4 |

- Acumulado de refactor tras A13c7: **−790** (`−502 − 288`).

### Cortes no aplicados

- S7c (fsproject): NO APLICADO — confirmado por el usuario («S7c no aplicado», 2026-08-09). No se identificó un hallazgo verificable con evidencia del repo; los candidatos (doble rejectSymlinkComponents, doble Close(), guard Windows de syncDirectory) implican cambios de comportamiento/seguridad que no se ejecutan sin especificación aprobada.
- S10-discovery (lspdiscovery): NO APLICADO — sin hallazgo verificable (isInside/computeSHA256 son lógica real con caller único, no wrappers de stdlib).
- S11 (openaillm): NO APLICADO — skip justificado durante la revisión del Commit 4 (5cb2d2c).
- cloneContents duplicada: REMANENTE — internal/usecase/contextcompiler/compiler.go y internal/adapter/adkagent/compaction.go conservan copias locales byte-idénticas de cloneContents (usadas en 4+2 sites); candidato futuro de consolidación a domain vía Content.Clone. No se aplicó en A13c7 por alcance del batch.

- Desviación deliberada (Commit 10): variable local maxInt en internal/usecase/contextcompiler/compiler.go:939 se conservó — fuera de alcance del batch.
- Nota de cierre: este commit documental no suma al acumulado de refactor (patrón establecido en A13c5/A13c6); su propio hash no puede insertarse en el contenido sin crear un commit adicional.

## A13c8 (post-A13c7)

### Cortes aplicados

| Commit | Shortstat | Neto | Decisión |
|---|---|---|---|
| `53943fbc` (cloneContents → `domain.CloneContents`) | `3 files changed, +14/-22` | −8 | Aplicado; 6 call sites actualizados, 2 copias locales eliminadas. |
| `8f2e7543` (S11: `^uint(0)>>1` → `math.MaxInt`) | `2 files changed, +5/-4` | +1 | **Desviación documentada**: el +1 proviene del import de `math`; se mantiene por decisión explícita del usuario (2026-08-09), consistente con S9/S10 ya aplicados. Excepción a la regla de neto < 0. |

- Acumulado de refactor tras A13c8: **−797**.
- Nota de cierre: este commit documental no suma al acumulado (patrón A13c5/A13c6); su propio hash no puede insertarse en el contenido sin crear un commit adicional.

## A13d

### Cortes aplicados

Ponytail-review: se aplicaron los ocho hallazgos siguientes:

| Hallazgo | Localización original | Veredicto y motivo |
|---|---|---|
| #1 `responsePlan` duplicaba `reduciblePart` | `internal/usecase/contextcompiler/analysis.go` | **APLICADO**: queda un solo `reduciblePart` y se conserva la condición `cost > minimumCost`. |
| #2 `compilationState` tenía 14 valores | `internal/usecase/contextcompiler/analysis.go` | **APLICADO**: se sustituyó por variables locales; solo cruzan fases los datos necesarios. |
| #3 `reduceResponses` tenía receptor y parámetros ignorados, y `codePointsRemoved` no se usaba | `internal/usecase/contextcompiler/allocation.go` | **APLICADO**: se reemplazó por `planProjections(parts, allocations)`. |
| #4 Las ramas de respaldo reconstruían response/JSON/digest | `internal/usecase/contextcompiler/externalization.go` | **APLICADO**: leen esos campos desde `reduciblePart`. |
| #5 `cloneMapShallow` era una clonación manual | `internal/usecase/contextcompiler/externalization.go` | **APLICADO**: se usa `maps.Clone` (Go 1.25.0). |
| #6 Había un bucle manual de inversión | `internal/usecase/contextcompiler/allocation.go` | **APLICADO**: se usa `slices.Reverse`. |
| #7 Se mantenían `stage`/`stageOrder`/`markStage` y `TestCompilerPhaseOrder` | `internal/usecase/contextcompiler/analysis.go`, `compiler_test.go` | **APLICADO**: se eliminó la instrumentación y la prueba de orden de fases. |
| #8 `projectionMutation.budget` se escribía y nunca se leía | `internal/usecase/contextcompiler/externalization.go` | **APLICADO**: se eliminó el campo. |

### Hallazgo clave del ítem 002

La prueba ADK de dos pasos demostró que ADK **NO persiste** la proyección del callback en el ledger: el segundo request sí la recibe. Por ello, la clasificación estricta de marcadores es segura y el escape hatch no se activó.

### Commits y acumulado

| Commit | Shortstat | Neto |
|---|---|---|
| `0a776a2e` | `+177/-3` | +174 |
| `5cba0147` | `+86/-43` | +43 |
| `d7908f4` | `+635/-159` | +476 |
| `8c2e6109` | `+1392/-934` | +458 |
| `b7c0e81` | `+105/-283` | −178 |

- Acumulado contabilizado de la cadena: **+756** (`+476 + 458 − 178`), según el corte de los ítems 002, 004 y 005. Los ítems 001 y 003 quedan registrados con netos +174 y +43, pero no se suman a este acumulado.
- El commit documental no suma al acumulado de refactor; su hash y shortstat se reportan en el cierre.
