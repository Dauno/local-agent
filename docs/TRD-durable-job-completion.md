# TRD: Entrega y continuación de jobs durables de agentes externos

**Estado:** Propuesto
**Prioridad:** P0 primero, P1 después
**Fecha:** 2026-08-01
**Alcance:** jobs ACP durables, especialmente `opencode_worker` y `sol-advisor`, con entrega en Slack

## 1. Resumen ejecutivo

Un job ACP durable termina fuera del turno del agente root. El host sí puede
publicar un resultado en el thread de Slack mediante el outbox durable, pero ese
mensaje lo publica el propio bot y el router lo descarta deliberadamente. Por
tanto, publicar un resultado no equivale a despertar al root. El usuario debe
escribir otro mensaje para iniciar un turno nuevo, y el texto observado
`OpenCode job <id> completed` no es un protocolo: solo coincidió con el contexto
que el modelo necesitaba.

Este TRD propone dos fases separadas:

1. **P0, garantía de entrega directa:** hacer verificable y observable el flujo
   terminal -> outbox -> Slack -> `published`. Un job terminal debe dejar un
   resultado visible, o un diagnóstico de entrega igualmente durable, sin
   depender de un mensaje adicional del usuario. Los errores del worker no se
   silencian; se registran con códigos seguros, métricas acotadas y una consulta
   administrativa local por job.
2. **P1, continuación tipada del root:** añadir un evento interno durable y
   deduplicado que invoque un nuevo turno root con identidad cargada del job.
   No es un mensaje Slack, no pasa por el router y no reconoce texto. La
   continuación es complemento de P0: si el root falla, el resultado directo de
   P0 ya debe estar disponible como fallback; nunca se reejecuta el ACP por esta
   causa.

La autorización de `job_status` y `read_job_result` permanece ligada al actor y
la conversación originales. La solución no acepta mensajes del bot como
triggers y no amplía permisos globales.

## 2. Contexto y problema

### 2.1 Flujo actual observado en el código

Los agentes `opencode_worker` y `sol-advisor` están definidos como `AcpAgent`
con `execution_mode: durable_job` en
`.local-agent/agents/opencode_worker.yaml:1-7` y
`.local-agent/agents/sol-advisor.yaml:1-6`. El root crea el job con el actor,
team y `ConversationKey` de la invocación actual en
`internal/app/agent_tools.go:345-356`. El job queda aceptado y el root puede
terminar su turno sin esperar al ACP.

El worker ACP persiste la transición terminal y no publica directamente. En
`internal/usecase/externalagent/service.go:310-341`, `execute` llama a
`ExternalAgentJobStore.Transition`; la entrega terminal queda a cargo del
worker de notificaciones independiente. La implementación SQLite:

- comprueba lease, intento, estado y revisión con compare-and-set en
  `internal/adapter/sqlite/external_agent_job_store.go:314-363`;
- inserta el evento de job y la notificación dentro de la misma transacción en
  `internal/adapter/sqlite/external_agent_job_store.go:364-372`;
- construye el delivery completo para un job detached completado en
  `internal/adapter/sqlite/external_agent_job_store.go:443-470`;
- usa `(job_id, status_revision, kind)` como clave primaria en la tabla creada
  por `internal/adapter/sqlite/migrate_v19.go:10-37`.

El contrato de entrega v1 conserva contenido, digest, tamaño, modo, artifact y
estado de upload. Está definido en
`internal/domain/external_agent_job.go:76-107` y construido por
`NewExternalAgentJobDelivery` en `internal/domain/external_agent_job.go:154-252`.
La base del checkout inspeccionado soporta schema **v26** (`SchemaVersion` en
`internal/adapter/sqlite/db.go:15`); v22 añadió los campos de delivery y v26
aplicó las invariantes de evidencia (`internal/adapter/sqlite/migrate_v22.go`
y `internal/adapter/sqlite/migrate_v26.go`). El diseño no debe asumir que v22
es la versión final.

La notificación se reclama con lease, se reconcilia cuando hace falta, verifica
la lectura host-owned y publica en Slack en
`internal/usecase/externalagent/notifications.go:72-103`. La verificación llama
a `HostCompletionTurn` en las líneas 105-131. El host completer es
intencionalmente determinista: `internal/usecase/externalagent/service.go:219-227`
lee el resultado autorizado y no invoca al modelo ni crea confirmaciones.

El publisher escribe el resultado en el canal/thread objetivo con metadata
durable en `internal/adapter/slack/job_notification.go:41-90`. Para Markdown,
la metadata incluye job, revisión, kind, digest, modo y política; para archivos,
`publishFile` sube el artifact privado y publica el estado en
`internal/adapter/slack/job_notification.go:218-300`.

### 2.2 Por qué el root no despierta

El host publica el mensaje, pero no existe un dispatch interno al root. El
listener solo entrega al handler las invocaciones resultantes del Events API en
`internal/adapter/slack/listener.go:390-418`; el handler configurado en
`internal/app/composition.go:818-825` llama únicamente a
`composition.service.Handle` para invocaciones Slack.

Además, el router excluye expresamente mensajes propios del bot y mensajes con
`BotID` en `internal/adapter/slack/router.go:145-167`. También rechaza, según
el contexto, mensajes de canal que no sean respuestas de thread en
`internal/adapter/slack/router.go:102-143`. Esta barrera es correcta: quitarla
permitiría loops, duplicados y spoofing. No se debe modificar para resolver
este problema.

El resultado observado, por tanto, tiene tres pasos independientes:

1. el job termina y el host materializa el resultado;
2. el outbox publica el mensaje visible en Slack;
3. ningún evento interno llama a `AgentRuntime.Run` para el root.

El mensaje manual del usuario inicia otro turno por el camino normal de
`bot.Service.Handle`, pero no es una señal de job parseada. Cualquier solución
que busque el ID o la frase `completed` en texto sería frágil y permitiría que
un usuario o un mensaje externo simule una finalización.

### 2.3 Observabilidad y autorización actuales

`NotificationWorker.Run` descarta el retorno de `ProcessOne` en
`internal/usecase/externalagent/notifications.go:57-67`. Además,
`recordFailure` puede ocultar el error de persistir `unknown` en las líneas
134-155. En un fallo de claim, de publicación o de la transición de estado no
hay una señal consistente de worker, log estructurado ni métrica específica.

El estado durable sí contiene los campos necesarios para diagnóstico:
`publish_state`, `attempts`, `last_error_code`, `delivery_mode`, lease,
`next_attempt_at` y `recovered_slack_ts` están en
`ExternalAgentJobNotification` (`internal/domain/external_agent_job.go:91-107`).
Sin embargo, `CheckExternalAgentJobStore` solo comprueba schema, tabla y siete
columnas de delivery en `internal/adapter/sqlite/store.go:645-671`; no detecta
notificaciones estancadas ni expone una consulta por job.

La autorización del root es deliberadamente estricta. `Service.Status` exige
actor y conversación no vacíos y compara ambos contra el job en
`internal/usecase/externalagent/service.go:157-168`. `read_job_result` pasa el
actor y la clave capturados al construir las tools en
`internal/adapter/toolfactory/toolfactory.go:816-893`. Las pruebas existentes
verifican actor incorrecto y conversación incorrecta en
`internal/usecase/externalagent/service_test.go:224-270` y
`internal/usecase/externalagent/authorization_test.go:13-34`.

La clave canónica se deriva según DM threaded/no threaded y thread de canal en
`internal/domain/invocation.go:114-135`; el router fija `ThreadedDM` según
configuración en `internal/adapter/slack/router.go:119-125`. El error observado
`external-agent job operation is not authorized` puede ser un mismatch real de
actor o `ConversationKey`, pero el código actual no registra cuál de los dos
falló. El diagnóstico debe distinguir ambos sin revelar sus valores y debe
corregir cualquier generación duplicada de claves sin mutar jobs ya persistidos.

### 2.4 Cobertura existente y brecha

Hay pruebas unitarias útiles, pero no el escenario solicitado de extremo a
extremo:

- `internal/adapter/sqlite/external_agent_notification_test.go:15-47` verifica
  transición terminal, una notificación y deduplicación del claim, pero no
  llama a Slack ni marca `published`;
- `internal/usecase/externalagent/notifications_test.go:163-186` prueba el
  completer determinista con un publisher fake;
- `internal/adapter/slack/job_notification_test.go:19-38` prueba metadata y
  publicación con un recorder aislado;
- `internal/adapter/slack/router_test.go:247-360` protege el rechazo de
  mensajes propios y otros eventos inseguros.

Falta una prueba integral que pruebe, en una misma ejecución, transición
terminal, inserción outbox, publicación Slack, evidencia y estado final
`published`, sin mensaje adicional del usuario.

## 3. Requisitos funcionales y no funcionales

### 3.1 Requisitos P0

**P0-F1, entrega directa durable.** Cada transición terminal de un job detached
debe insertar exactamente un delivery identificado por
`(job_id, status_revision, kind)` en la misma transacción que la transición del
job. Un job `completed` debe producir un mensaje Markdown multipart o un
artifact Slack visible en el destino persistido. Si la publicación no puede
probarse, debe quedar un diagnóstico de entrega durable y reintentable según su
código; nunca se debe considerar publicado sin `recovered_slack_ts` y, para
file mode, file ID y upload completado.

**P0-F2, estado observable.** La consulta y el diagnóstico deben exponer, como
mínimo, `publish_state`, `attempts`, `last_error_code`, `delivery_mode`,
`status_revision`, `next_attempt_at`, lease/expiración, estado de upload y
`recovered_slack_ts`. No se exponen task, contenido completo, path de artifact,
tokens ni cuerpos de error del proveedor.

**P0-F3, errores no silenciosos.** `NotificationWorker.Run` debe registrar cada
error retornado por `ProcessOne`. `recordFailure` debe retornar cualquier error
al actualizar el estado durable; una falla de persistencia no puede convertirse
en `nil`. Los errores de proveedor se clasifican con códigos ya acotados, sin
guardar respuesta raw, secretos o resultado, y se registran/contabilizan cuando
la transición durable del fallo sí fue persistida. `ProcessOne` solo puede
retornar `nil` después de haber dejado una decisión durable o no haber tenido
trabajo.

**P0-F4, salud y métricas.** Debe existir un snapshot de salud del outbox con
conteos de `pending`, `publishing`, `unknown`, `published`, fallos permanentes
y notificaciones estancadas. Se considera estancada una publicación cuyo lease
expiró o cuyo `next_attempt_at` venció más allá del umbral configurado. Deben
emitirse contadores de claim, éxito, fallo, reconciliación y conflicto CAS, y
un gauge de estancadas. Las etiquetas son solo dimensiones acotadas como
`result_kind`, `delivery_mode` y `failure_category`; nunca job ID, actor,
conversation key, task, digest o contenido.

**P0-F5, consulta administrativa segura.** Añadir una operación local,
preferiblemente `local-agent jobs inspect <job_id>`, de solo lectura. Debe
permitir inspeccionar un job y su delivery por ID sin requerir actor Slack ni
llamar al modelo. Un job inexistente devuelve un resultado seguro y no revela
si existe otro job. La salida muestra estado, revisión, tiempos, intentos,
modo, código de error y estado del outbox; omite task, result text, artifact
path, credenciales y valores completos de actor/conversación.

**P0-F6, diagnóstico del mismatch.** En `Status`, registrar/contabilizar
`actor_match` y `conversation_match` como booleanos acotados antes de devolver
el mismo error de autorización. No incluir en logs los valores comparados. La
generación de `ConversationKey` debe tener una única fuente de verdad para la
invocación Slack y el request de job. Deben cubrirse DM threaded, DM legacy,
thread de canal y continuación del mismo thread. Jobs ya persistidos conservan
su clave original: si una clave es inválida o antigua, se diagnostica y no se
reescribe automáticamente.

**P0-F7, independencia del router.** La entrega P0 no depende de Events API,
de `Router.Route`, de mensajes del bot, de menciones ni de parsing textual.
Los mensajes propios deben seguir siendo ignorados por
`Router.ignoreMessage`.

**P0-F8, aislamiento de fallos.** Un fallo de P1, del modelo root o de una
continuación nunca puede impedir que el worker P0 publique o reintente el
resultado directo. P0 no invoca al modelo.

### 3.2 Requisitos P1

**P1-F1, evento interno tipado.** Para cada `(job_id, status_revision,
root_continuation)` elegible debe existir una entrada durable única. El evento
se genera en la transición terminal y no se deduce de Slack ni de texto. Debe
contener solo el job ID y revisión como identidad de lookup; actor, team y
`ConversationKey` se cargan desde SQLite al procesar, nunca desde argumentos del
modelo ni de un mensaje generado.

**P1-F2, API de continuación.** Exponer un handler de aplicación con forma
`HandleExternalJobCompletion(ctx, jobID, statusRevision)` que:

1. cargue y valide el job completado y la entrega P0 desde el store;
2. compruebe que la revisión coincide y que el delivery directo está probado;
3. adquiera el lock de la conversación usando la clave del job;
4. ejecute un nuevo turno del root mediante una entrada tipada;
5. publique y finalice la respuesta con correlación durable;
6. marque el evento completado mediante CAS de owner/attempt.

No debe aceptar actor, conversación, task, provider, aprobación o destino desde
el payload que emita el modelo.

**P1-F3, resultado Markdown.** Para modo Markdown, el turno root recibe el
resultado ya sanitizado y redacted, con digest, tamaño, job ID y revisión como
metadata host-owned. El contenido debe respetar el límite de contexto y no
convertirse en un mensaje Slack simulado ni en una instrucción de autorización.

**P1-F4, resultado file.** Para modo file, P0 entrega el artifact directamente
al thread. La continuación root recibe únicamente metadata segura: job ID,
revisión, modo file, tamaño, digest y estado de entrega. Nunca recibe artifact
path, referencia privada, bytes completos ni URL interna. Si el root falla, el
archivo de P0 sigue siendo el fallback visible.

**P1-F5, idempotencia y crash recovery.** La clave única del evento y una
correlación determinista, por ejemplo
`job:<job_id>:<status_revision>:root_continuation`, deben impedir dos
continuaciones lógicas. La publicación del root debe usar el patrón existente
de `AssistantExchangeWriter` (`internal/port/conversation.go:27-43`) y
reconciliación por metadata, para que un crash después de Slack y antes del
CAS no publique una respuesta duplicada.

**P1-F6, fallback obligatorio.** Si la ejecución del root, la construcción del
turno o su publicación falla, el worker marca un código seguro de continuación
fallida y conserva P0. Si P0 aún no está publicado, la continuación no puede
declararse fallida como sustituto: debe dejar/reencolar el delivery directo o
esperar su recuperación. Nunca se reejecuta la tarea ACP.

**P1-F7, serialización por conversación.** Una continuación que compite con un
turno Slack normal debe esperar/reintentarse de forma durable, no publicar un
mensaje de ocupado ni ejecutar dos turnos root simultáneos para la misma clave.
Debe usar el mismo límite de llamadas de modelo del proceso.

**P1-F8, confirmaciones.** La continuación procesa un resultado ya admitido y
no puede aprobar acciones ni crear una segunda confirmación implícita. Si el
turno typed produce `PendingConfirmation`, el worker conserva P0, registra
`root_continuation_confirmation_required` y marca la continuación como fallo
accionable; cualquier aprobación posterior debe pasar por el flujo de
confirmación durable existente y una nueva política explícita.

### 3.3 Requisitos no funcionales

**P0-NF1, seguridad.** Mantener autorización estricta de `job_status`,
`read_job_result`, cancelación y reconciliación. La consulta administrativa es
un canal local de operador separado, no una nueva capacidad del modelo.

**P0-NF2, durabilidad.** Claims, leases, reintentos, evidencia Slack y
continuaciones deben sobrevivir reinicio. Las escrituras de estado usan CAS
con job, revisión, owner y attempts.

**P0-NF3, bounded output.** Mantener sanitización, redacción, límites de bytes y
code points, multipart Markdown y artifacts privados 0600 existentes. Nunca
registrar contenido de resultados.

**P0-NF4, compatibilidad.** La migración P1 será aditiva sobre schema v26. Las
filas `legacy_v1` existentes no se convierten silenciosamente ni se vuelven a
reproducir. No se requiere `init --reset-state` para añadir health, admin o la
tabla de continuaciones.

**P0-NF5, operación.** El proceso debe arrancar con workers correctamente
compuestos o fallar de forma explícita. Un worker que no puede persistir su
estado no debe continuar como si hubiera entregado.

## 4. Diseño propuesto

### 4.1 Componentes

**Delivery outbox P0.** Se conserva `external_agent_job_notifications` como
fuente de verdad de la entrega Slack. Su key, metadata, leases y estados no se
reemplazan. Se agregan métodos de inspección/health y observabilidad al port y
al store SQLite.

**NotificationWorker P0.** Recibe `port.Logger` y `port.MetricRecorder`. Cada
iteración reclama una fila, verifica el resultado host-owned, publica o
reconcilia y marca por CAS. Una falla del provider se persiste como código; una
falla de claim o de persistencia retorna error y se registra con job ID,
revisión, kind, attempts y código, pero sin contenido.

**Completion outbox P1.** Se añade una tabla, por ejemplo
`external_agent_job_continuations`, con:

| Campo | Propósito |
|---|---|
| `job_id`, `status_revision`, `continuation_kind` | identidad única del evento |
| `state` | `pending`, `processing`, `completed`, `failed` |
| `attempts`, `lease_owner`, `lease_expiry`, `next_attempt_at` | claim y retry |
| `last_error_code` | diagnóstico seguro |
| `created_at`, `updated_at` | edad y health |

La inserción usa `ON CONFLICT DO NOTHING` dentro de la misma transacción de
`Transition` que inserta el delivery P0. Solo se crean eventos para jobs
detached `completed` con delivery válido. Un estado terminal de fallo sigue
teniendo entrega P0, pero no despierta el root para procesar un resultado que no
existe.

**CompletionWorker P1.** Es independiente del publisher Slack. Reclama la
entrada, verifica que el delivery P0 esté `published`, y llama al handler
tipado. Si P0 está `pending`, `publishing` o `unknown`, reencola con backoff y
no procesa bytes. Si P0 queda en fallo permanente, mantiene la continuación en
estado diagnóstico sin ocultar el fallo directo.

**Handler root P1.** Vive en el use case bot o en un servicio de aplicación
compuesto por `internal/app`, no en el adapter Slack. Su API recibe solo job ID
y revisión. Obtiene actor, team, conversación y resultado del store confiable,
usa el lock por `ConversationKey`, y llama a una operación tipada del runtime
ADK. No llama a `HostCompletionTurn` como si fuera un turno de modelo: ese
componente sigue siendo la ruta determinista P0.

**Runtime ADK P1.** `internal/adapter/adkagent/runtime.go` debe aceptar una
solicitud de continuación typed, o un campo equivalente en
`port.AgentRequest`, y construir un evento host-owned dentro de la sesión ADK.
No debe crear `domain.Invocation`, `EventID` Slack ni mensaje de usuario para
pasarlo por el router. El evento debe conservar job ID/revisión en metadata
durable y suministrar al modelo únicamente el resultado permitido por el modo
de delivery.

### 4.2 Flujo P0

1. El tool ACP construye `ExternalAgentJobRequest` con el `ConversationKey`
   actual (`internal/app/agent_tools.go:349-356`) y el job queda `queued`.
2. El worker externo ejecuta el ACP y materializa un resultado sanitizado; para
   resultados grandes crea artifact privado (`internal/app/external_agent_jobs.go:93-164`).
3. SQLite hace CAS de la transición terminal y, antes del commit, inserta una
   fila outbox única.
4. `NotificationWorker` reclama la fila y conserva `Actor` y
   `ConversationKey` obtenidos del job al cargar la notificación.
5. `HostCompletionTurn` revalida actor, conversación, tamaño y digest. No
   invoca ACP, ADK ni confirmaciones.
6. `JobNotificationPublisher` publica Markdown completo/multipart o sube y
   comparte el archivo en el target original, con metadata de correlación.
7. Solo con evidencia suficiente se marca `published`. Un error conserva
   `pending`, `unknown` o estado diagnóstico según la clasificación y actualiza
   health/métricas.

Este flujo no genera ningún evento que `Router.Route` deba aceptar. El mensaje
del bot sigue siendo visible para el usuario, pero no es una entrada del root.

### 4.3 Flujo P1

1. La misma transacción terminal inserta la fila de continuación única.
2. P0 publica y marca `published` de forma independiente.
3. `CompletionWorker` reclama el evento y carga el job y delivery por ID/revisión.
4. El handler adquiere el lock de `ConversationKey`. Si hay otro turno, hace
   retry durable.
5. Para Markdown, construye un evento host-owned con texto sanitizado,
   digest/tamaño y referencia lógica al job. Para file, construye solo metadata
   de entrega; el archivo ya fue enviado por P0.
6. El runtime root ejecuta un turno ADK en la sesión `adk:<ConversationKey>` con
   la identidad cargada del job. El contexto de job se marca como continuación
   para evitar que un `job_status` del root sea confundido con una nueva
   admisión o que un tool lance recursivamente el mismo job.
7. La respuesta root se prepara con correlación determinista, se publica en el
   mismo thread y se finaliza mediante el exchange durable existente.
8. El evento se marca `completed` por CAS. Un crash o timeout deja lease
   recuperable; una respuesta ya evidenciada por correlación se reconcilia sin
   duplicar.
9. Si cualquier paso root falla, se marca `root_continuation_failed` y se deja
   constancia segura. P0 sigue siendo el resultado visible/fallback y el ACP no
   se vuelve a ejecutar.

### 4.4 Cambios por archivo

#### P0

- `internal/port/external_agent_job.go`: agregar contratos de health,
  inspección administrativa y diagnóstico de autorización; extender
  dependencias del worker con logger/metrics sin acoplar el port a Slack.
- `internal/usecase/externalagent/notifications.go`: inyectar logger y
  recorder, eliminar retornos silenciosos de `Run`, devolver errores de
  persistencia de `recordFailure`, emitir contadores/gauges y snapshot de
  salud sin contenido.
- `internal/usecase/externalagent/service.go`: conservar la comparación estricta
  actor/conversación y registrar `actor_match`/`conversation_match`; centralizar
  la carga trusted usada por status, read y completion.
- `internal/domain/invocation.go`: extraer o consolidar el helper de identidad
  canónica si el diagnóstico demuestra rutas duplicadas; no cambiar la semántica
  de DM threaded/no threaded sin pruebas de compatibilidad.
- `internal/adapter/sqlite/external_agent_job_store.go`: añadir consulta de
  delivery por job, conteos de salud, oldest age y cualquier CAS requerido para
  diagnóstico. Nunca devolver task/result content en el view administrativo.
- `internal/adapter/sqlite/store.go`: ampliar la comprobación offline para
  detectar shape inválido o outbox estancado según el contrato definido, sin
  leer ni imprimir resultados.
- `internal/adapter/slack/job_notification.go`: mantener la publicación y
  evidencia actuales; ajustar solo lo necesario para que metadata y códigos
  sean suficientes para health/reconciliación.
- `internal/domain/context_metrics.go` y
  `internal/adapter/metrics/recorder.go`: añadir nombres de métricas y permitir
  únicamente etiquetas bounded (`result_kind`, `delivery_mode`,
  `failure_category`) mediante la allowlist existente.
- `internal/cli/root.go`, `internal/app/application.go` y una nueva capa de
  consulta administrativa en `internal/app`/`internal/adapter/sqlite`: añadir
  `jobs inspect`, abrir la DB en modo existente/read-only cuando sea posible y
  presentar solo el view seguro.
- `internal/adapter/slack/router.go`: no cambiar la exclusión de mensajes del
  bot; añadir regresiones explícitas si el wiring nuevo amenaza esa barrera.

#### P1

- `internal/domain/external_agent_job.go`: definir el evento typed de completion,
  su modo de resultado seguro y el estado/código de continuación. No incluir
  artifact path ni identidad suministrada por el modelo.
- `internal/port/external_agent_job.go`: agregar store de continuaciones,
  claim/CAS, consulta trusted de completion y handler
  `HandleExternalJobCompletion(ctx, jobID, statusRevision)`.
- `internal/adapter/sqlite/migrate_v27.go`: crear la tabla de continuaciones,
  constraints, índices de claim y key única. Registrar v27 en `migrate.go`,
  subir `SchemaVersion` en `db.go` y agregar v26 al conjunto de upgrades seguros.
- `internal/adapter/sqlite/external_agent_job_store.go`: insertar la
  continuación en la transacción terminal, reclamarla con lease, verificar
  revisión y marcar estados con owner/attempt CAS.
- `internal/usecase/externalagent/completions.go` (nuevo): implementar el
  worker P1, gating sobre P0 `published`, backoff, reconciliación y métricas.
- `internal/usecase/bot/service.go`: añadir el handler root typed, usar el lock
  por conversación y el límite global de modelo, reutilizar finalización y
  `AssistantExchangeWriter`, y dejar P0 como fallback sin publicar un mensaje
  de error que sustituya el resultado.
- `internal/port/agent.go` y `internal/adapter/adkagent/runtime.go`: añadir la
  operación/campo typed de continuación y su representación ADK durable; no
  simular `Invocation` ni pasar por `Router`.
- `internal/app/external_agent_jobs.go` y `internal/app/composition.go`:
  componer store, workers, logger, metrics y handler. Arrancar el
  `CompletionWorker` después de construir `bot.Service`; el worker P0 puede ser
  independiente, pero ninguno debe quedar sin dependencia explícita.
- `internal/adapter/slack/job_notification.go`: reutilizar la correlación
  durable del publisher para P0 y, si aplica, proveer reconciliación del mensaje
  de respuesta root sin usar eventos Slack como input.

### 4.5 Health, métricas y administración

Nombres propuestos:

| Métrica | Tipo | Etiquetas permitidas |
|---|---|---|
| `external_agent_notification_claim_total` | counter | `result_kind` |
| `external_agent_notification_publish_total` | counter | `delivery_mode` |
| `external_agent_notification_failure_total` | counter | `failure_category`, `delivery_mode` |
| `external_agent_notification_reconcile_total` | counter | `delivery_mode` |
| `external_agent_notification_cas_conflict_total` | counter | `result_kind` |
| `external_agent_notification_stuck` | gauge | ninguna |
| `external_agent_continuation_total` | counter | `outcome` |

El logger usa mensajes como `external-agent notification processing failed` con
`job_id`, `status_revision`, `kind`, `attempts` y `error_code`. No se incluyen
`CanonicalMarkdown`, task, artifact locator, URLs, provider response, actor ni
conversation key.

`local-agent jobs inspect <job_id>` debe mostrar una vista equivalente a:

```text
job_id: job_...
status: completed
status_revision: 4
finished_at: 2026-08-01T...
delivery_mode: markdown
notification_kind: terminal
publish_state: published
attempts: 1
last_error_code:
next_attempt_at:
recovered_slack_ts: 171...
```

Para file mode se muestra `upload_state` y si existe una identidad Slack, pero
no el contenido, path privado o URL de upload. El comando es local y de solo
lectura; no reemplaza la autorización de las tools del root.

## 5. Criterios de aceptación verificables

### P0

1. Un test de integración crea un job detached, lo ejecuta hasta `completed`,
   inspecciona una fila outbox única y, sin publicar ningún mensaje de usuario,
   observa una llamada Slack al canal/thread correcto.
2. Tras la llamada Slack exitosa, la fila queda `publish_state=published`, tiene
   `recovered_slack_ts` no vacío, mantiene el digest/revisión esperados y no
   vuelve a ser claimable.
3. El test de entrega file verifica URL, bytes, completion, file share y mensaje
   de estado; un reinicio en cada etapa reanuda sin crear un segundo archivo.
4. Un error de Slack incrementa attempts, conserva `last_error_code` seguro y
   programa retry o diagnóstico permanente. El test verifica log/metric y que
   `ProcessOne` no retorna silenciosamente un error de persistencia.
5. Una fila con lease `publishing` expirado aparece en health como estancada y
   es reclamable; una fila publicada no aparece como estancada.
6. `jobs inspect` muestra `publish_state`, `attempts`, `last_error_code` y
   `delivery_mode`, pero nunca task, result text, artifact path, token o cuerpo
   de error raw.
7. Un mismatch solo de actor y uno solo de conversación producen
   `not authorized`, pero registran métricas/logs con los pares booleanos
   correctos y sin valores sensibles.
8. La misma invocación original y su reply en el mismo DM/thread generan la
   misma clave canónica en modo threaded; los casos legacy y channel thread
   conservan su forma definida. Un job almacenado con una clave antigua no se
   reescribe automáticamente.
9. `Router` continúa ignorando mensajes del bot, incluidos los mensajes P0 con
   metadata de job; ningún texto `completed` inicia el root.

### P1

10. Una transición terminal crea como máximo una continuación para
    `(job_id, status_revision, root_continuation)` incluso tras retry, reinicio
    o doble notificación.
11. El completion worker procesa solo después de que P0 esté probado como
    `published`; no llama a `Router.Route` ni publica un mensaje Slack sintético.
12. El handler carga actor y conversación desde SQLite. Un actor/conversación
    alterado en un payload de prueba no puede cambiar el destino ni autorizar
    `job_status`/`read_job_result`.
13. Para Markdown el root recibe el texto sanitizado y metadata esperada; para
    file recibe metadata de tamaño/digest/modo y nunca bytes, path o referencia
    privada.
14. Si el runtime root falla, la notificación P0 sigue visible, el evento queda
    `root_continuation_failed` con retry/diagnóstico según política y el ACP no
    se ejecuta de nuevo.
15. Si el proceso muere después de publicar la respuesta root pero antes del
    mark-CAS, la reconciliación por correlación encuentra una sola respuesta y
    no duplica el mensaje.
16. Un turno Slack concurrente y una continuación para la misma clave no corren
    simultáneamente. La continuación reintenta de forma durable sin publicar
    un mensaje de ocupado.
17. Si el runtime devuelve `PendingConfirmation` durante una continuación, no
    se crea una aprobación automática ni se pierde P0; la fila queda con el
    código `root_continuation_confirmation_required`.

## 6. Riesgos y trade-offs

**Respuesta duplicada de P1.** Slack no ofrece exactly-once. Mitigación:
correlación determinista, exchange preparado antes de publicar, reconciliación
por metadata y CAS de continuación. El riesgo residual de una respuesta cuyo
estado externo sea ambiguo debe quedar en `unknown`, no reejecutar el ACP.

**Entrega directa y respuesta root duplican visibilidad.** P0 publica el
resultado completo por garantía; P1 puede publicar una síntesis o respuesta del
root. Esto es intencional: P1 no puede reemplazar P0. La UX debe rotular la
respuesta root como continuación y no recrear el artifact.

**Competencia por la sesión ADK.** Un turno de usuario puede llegar mientras el
worker procesa. El lock por conversación reduce corrupción de orden; reintentar
la continuación aumenta latencia, pero es preferible a dos turnos concurrentes.

**Mismatch de claves históricas.** Cambiar la función canónica puede reparar
nuevos jobs y romper lectura de antiguos. Antes de cambiarla se debe
instrumentar el caso, consolidar una única fuente de verdad y conservar la
clave persistida; nunca hacer una migración masiva inferida desde Slack.

**Backlog o proveedor Slack caído.** La publicación no puede ser sincrónica con
el turno root. El outbox permite retry y health; un backlog se hace visible por
gauge, consulta administrativa y log acotado. No se debe bloquear el worker ACP
por una llamada Slack lenta.

**Métricas in-process.** El repositorio tiene `port.MetricRecorder` y recorder
bounded, pero no un endpoint externo visible en el código inspeccionado. En P0,
logs, snapshot y `jobs inspect` son la superficie mínima; exponer Prometheus u
otro endpoint queda fuera de este TRD salvo que la operación lo requiera.

**Contenido sensible en la continuación.** Markdown sanitizado todavía puede
contener datos de repositorio. Se limita por la política existente, se redacta
antes de persistir/enviar y se evita logging. File mode reduce el contenido que
entra en la sesión root, a cambio de que el root solo conozca metadata.

**Migración de schema.** P1 requiere una migración aditiva v27 sobre v26. Debe
seguir el patrón de `migrate.go`; un schema futuro o una migración incompleta
debe fallar en startup, no iniciar workers parcialmente.

## 7. Plan de implementación por fases

### Fase P0: garantía y diagnóstico

1. Confirmar con pruebas actuales el contrato de transición, delivery,
   publisher, router y autorización; fijar los casos de key canónica.
2. Añadir logger/metrics al `NotificationWorker` y hacer visibles errores de
   claim, publish, reconcile y persistencia de estado.
3. Añadir vista administrativa por job y snapshot de salud de outbox; integrar
   la consulta local y el diagnóstico offline sin exponer contenido.
4. Instrumentar `Status` con `actor_match` y `conversation_match`; reproducir
   el mismatch observado usando DM threaded, DM legacy y channel thread.
5. Consolidar la generación de `ConversationKey` solo si la evidencia muestra
   más de una fuente; mantener las claves persistidas y reforzar pruebas.
6. Añadir el test integral terminal -> outbox -> Slack -> `published` y el
   equivalente de reinicio/error de file mode.
7. Verificar health, retries, CAS conflict, router-ignore y autorización.
8. Desplegar P0 y observar backlog, fallos y claves antes de activar P1.

### Fase P1: continuación typed del root

1. Definir el evento y los estados de continuación en domain/port, incluyendo
   límites y metadata permitida por modo de delivery.
2. Añadir migración v27 y store con inserción atómica, leases, retries y CAS.
3. Implementar `CompletionWorker` gated por P0 publicado; incluir métricas y
   códigos `root_continuation_*`.
4. Extender runtime ADK con evento typed durable, sin `Invocation` Slack ni
   mensaje sintético.
5. Implementar el handler en bot use case con lock de conversación, límite de
   modelo y reutilización de finalización/exchange.
6. Componer el handler y arrancar el worker después del root service, sin
   acoplar `externalagent` a `bot`.
7. Probar Markdown, file mode, fallo root, retry, restart y dos eventos
   concurrentes.
8. Activar P1 después de demostrar que P0 entrega incluso cuando el root está
   caído o no responde.

## 8. Estrategia de pruebas

### Unitarias

- `internal/usecase/externalagent/notifications_test.go`: `Run` registra
  errores, `recordFailure` propaga fallo de persistencia, métricas no contienen
  contenido y health clasifica lease expirado.
- `internal/usecase/externalagent/service_test.go` y
  `authorization_test.go`: matriz actor correcto/incorrecto, key correcta,
  key incorrecta, mismatch doble; probar que el mensaje de error público no
  cambia y que las señales internas distinguen ambos booleanos.
- `internal/domain/invocation_test.go`: DM legacy, DM threaded root, DM
  threaded reply, channel root y channel reply; igualdad de key y target.
- `internal/adapter/sqlite/external_agent_notification_test.go`: unicidad,
  transitions, attempts, `last_error_code`, health y admin view; mantener
  rollback cuando el delivery no se puede construir.
- `internal/adapter/slack/job_notification_test.go`: metadata completa,
  digest, multipart, file upload state y evidencia de publicación.
- `internal/adapter/slack/router_test.go`: mensajes P0 del propio bot y
  mensajes con metadata de job siguen siendo ignorados.
- `internal/usecase/bot` y `internal/adapter/adkagent`: lock por key,
  evento typed, modo file sin bytes y reuso de exchange/correlation.

### Integración P0

Crear un test en `internal/integration`, con SQLite temporal y un servidor HTTP
local que implemente las respuestas mínimas de Slack. El escenario debe:

1. crear un job detached con actor/team/key reales;
2. reclamarlo y hacer `Transition(JobCompleted, result)`;
3. verificar en DB el job terminal y el outbox único antes de publicar;
4. ejecutar una iteración real de `NotificationWorker` con publisher durable;
5. comprobar que Slack recibió el canal/thread correcto y metadata job/revisión
   y digest;
6. comprobar `published`, timestamp recuperado y ausencia de filas duplicadas;
7. no enviar ningún evento `MessageEvent` de usuario ni invocar el runtime root.

Añadir variantes de fallo HTTP, reinicio con `publishing` expirado, respuesta
ambigua, delivery multipart y file mode. En file mode, comprobar la secuencia
durable URL -> bytes -> completion -> status, y que el artifact no se borra
mientras exista una referencia de delivery no publicada.

### Integración P1

Usar un runtime root fake que capture el evento typed y un publisher que exponga
correlación y timestamps. Verificar:

- transición terminal produce P0 y una sola continuación;
- P1 espera `published` y no usa router;
- actor/key llegan desde la fila job, no desde el evento mutable;
- Markdown transporta solo resultado sanitizado y file mode solo metadata;
- lock bloqueado reencola sin segundo turno;
- error root deja P0 publicado y marca continuación con código seguro;
- crash entre publicación y CAS se reconcilia a una sola respuesta;
- retries de la misma tupla no reejecutan ACP ni generan un segundo mensaje.

### Verificaciones de aceptación del repositorio

Antes de integrar cada fase ejecutar:

```sh
go build ./...
go vet ./...
go test ./...
git diff --check
```

El build/test debe incluir la comprobación arquitectónica de dependencias y no
introducir una importación de adapters desde `internal/usecase` o
`internal/domain`. No se requieren credenciales Slack ni llamadas a servicios
reales.
