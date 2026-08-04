# Plan de ejecucion: admision multimodal de imagenes

## Objetivo

Implementar `TRD.md` en incrementos P0/P1/P2. P0 corrige el incidente end-to-end: normaliza antes del Artifact, elimina base64 del payload contable, activa un estimator visual versionado y demuestra `load_artifacts -> request visual admitido -> descripcion textual`. P1 agrega capacidad/configuracion, doctor, metricas y fuzzing. P2 mejora precision y formatos sin debilitar P0.

## Precondiciones

- Inspeccionar `git status` y preservar cambios preexistentes.
- Confirmar el perfil real de `attachment_analyzer`, su `context_window_tokens`, hard budget efectivo y soporte de data URLs; no copiar secretos al issue, logs o fixtures.
- Mantener Go 1.25, ADK v2.0.0 y OpenAI Go v3.43.0 salvo cambio separado y justificado.
- Revisar licencia, advisories, API y compatibilidad Go de cualquier dependencia de imagen; fijar versiones explicitas y ejecutar `go mod tidy` solo despues de seleccionar las versiones.
- No cambiar texto/audio/root, schema SQLite, Slack scopes ni durable sessions.
- Usar fixtures sin datos personales, EXIF sensible, credenciales o contenido propietario.

## Estrategia de entrega

```text
P0-A reproducer + normalizador
  -> P0-B envelope multimodal + estimator
    -> P0-C wiring + errores + integracion + docs
      -> P1 capacidades + operacion/hardening
        -> P2 precision y formatos avanzados
```

Cada incremento debe quedar compilable y con tests enfocados verdes. Tests de regresion se agregan junto con el comportamiento que los hace pasar; el fallo previo puede documentarse localmente, pero no se entrega una rama intencionalmente roja.

## Dependencias

- `internal/adapter/adkartifact` posee normalizacion y Artifact visual.
- `internal/adapter/openaillm` posee conversion tipada real/contable.
- `internal/port/model_context.go` posee metadata provider-neutral del envelope.
- `internal/adapter/tokencounter` posee formulas y IDs concretos.
- `internal/app` resuelve perfil, compone counter y pasa opciones.
- `internal/agentdef` y `internal/config` poseen schema/validacion declarativa.
- `internal/usecase/doctor` valida disponibilidad y configuracion.
- `internal/domain/context_metrics.go` y `internal/adapter/metrics` poseen nombres y labels bounded.
- Dependencias nuevas candidatas: `github.com/disintegration/imaging` y `golang.org/x/image/webp`, pure Go y versionadas tras revision.

## P0: correccion obligatoria

### Task P0-01

- ID: `P0-01-realistic-regression-fixture`
- Objetivo: fijar el incidente con una imagen real decodificable que base64+byte-bound llevaria por encima de 65.536.
- Probables archivos/componentes: `internal/adapter/adkartifact/testdata/` o `internal/integration/testdata/`, `processor_test.go`, `openaillm/llm_test.go`.
- Pasos:
1. Agregar un fixture sanitizado de 550-700 KiB, con dimensiones representativas y contenido tipo screenshot/fotografia que permita verificar width/height.
2. Calcular en test la serializacion legacy con data URL y demostrar que `len(serialized) > 65_536`; no fijar exactamente 800.466 porque depende del fixture.
3. Definir assertions objetivo: normalizacion bounded, envelope sin base64 y request admitido bajo un budget de 65.536.
4. Evitar snapshots gigantes y no imprimir base64 en fallos.
- Dependencias: ninguna.
- Validacion: fixture decodifica; test demuestra la condicion legacy y pasa solo con la nueva ruta completa del paquete P0-A/P0-B.
- Condicion de completado: existe una regresion estable que falla con la arquitectura anterior por inflacion de base64 y protege el resultado nuevo.

### Task P0-02

- ID: `P0-02-image-normalizer-core`
- Objetivo: validar contenido y producir un derivado canonico acotado antes de Artifact.
- Probables archivos/componentes: nuevo `internal/adapter/adkartifact/image.go`, nuevo `image_test.go`, `go.mod`, `go.sum`.
- Pasos:
1. Implementar sniff por `image.DecodeConfig` con decoders JPEG/PNG/GIF/WebP registrados.
2. Validar dimensiones positivas, edge fuente <= 32.768 y producto `int64` <= 40.000.000 antes de decode completo.
3. Revisar y fijar dependencias pure Go; no usar encoder WebP ni CGO.
4. Decodificar una vez, aplicar orientacion EXIF y eliminar metadata mediante recodificacion.
5. Implementar resize aspect-ratio, sin upscale, con max edge 1.568.
6. Canonizar JPEG a calidad 85; PNG permanece PNG; WebP/GIF usan PNG con alpha o JPEG sin alpha.
7. Limitar resultado a 2 MiB con niveles `1568,1344,1152,1024,768,512`, maximo seis encodes y sin redecode.
8. Devolver MIME, extension, dimensiones y warning GIF desde una estructura privada.
- Dependencias: `P0-01` para fixture de regresion.
- Validacion: table tests para dimensiones, no upscale, aspect ratio, bytes, formato, alpha, EXIF, WebP, GIF, cancelacion y limites.
- Condicion de completado: todo input aceptado produce un derivado <= 2 MiB y <= 1.568 edge; todo input hostil falla antes de Artifact/model.

### Task P0-03

- ID: `P0-03-normalize-before-artifact`
- Objetivo: guardar el derivado normalizado con identidad canonica sin alterar texto/audio.
- Probables archivos/componentes: `internal/adapter/adkartifact/processor.go`, `processor_test.go`, helper de nombre seguro.
- Pasos:
1. Separar clasificacion para que la ruta visual normalice antes de `genai.NewPartFromBytes` y `Artifact.Save`.
2. Usar formato real como autoridad cuando el MIME declarado sea visual pero difiera de otro formato soportado.
3. Construir nombre de Artifact con stem saneado y extension `.jpg`/`.png` canonica.
4. Conservar la ruta actual de bytes originales para texto y audio.
5. Reemplazar el test de bytes `png` falsos por imagenes reales y capturar el `Part` guardado para verificar MIME, dimensiones, bytes y nombre.
6. Agregar warning determinista al resultado de GIF primer-frame.
- Dependencias: `P0-02`.
- Validacion: tests del processor prueban que el analyzer ve el derivado y que corrupt/mismatch/oversize no llaman Save ni model.
- Condicion de completado: ningun Artifact visual contiene bytes originales; texto y audio conservan tests existentes sin cambios de conducta.

### Task P0-04

- ID: `P0-04-multimodal-envelope`
- Objetivo: construir en forma tipada un request HTTP real y un payload contable sin binario.
- Probables archivos/componentes: `internal/port/model_context.go`, `internal/adapter/openaillm/convert.go`, `llm.go`, `llm_test.go`, `convert_protocol_test.go`, fakes que implementan `RequestTokenCounter`.
- Pasos:
1. Agregar `ModelRequestMedia{MIMEType,Width,Height,Detail}` y `Media` al envelope.
2. Refactorizar la conversion minima para producir parametros reales y proyeccion contable en una pasada, preservando orden de partes.
3. Sustituir cada data URL solo en la proyeccion tipada por `local-agent://media/omitted`; no usar regex ni reemplazo sobre JSON.
4. Obtener width/height con `DecodeConfig` del derivado y fallar si la parte visual no es valida.
5. Versionar serializer multimodal y mantener serializer/conteo actual para requests sin media.
6. Pasar proyeccion y media al guard antes de HTTP; usar parametros reales solo tras admission.
7. Actualizar todos los fakes/contract tests por el campo aditivo.
- Dependencias: `P0-03` define derivado canonico.
- Validacion: JSON contable no contiene base64 ni bytes; body HTTP si contiene data URL; orden, tools y texto permanecen identicos.
- Condicion de completado: no existe ruta de media que llegue al counter con base64 ni ruta HTTP que use el marcador.

### Task P0-05

- ID: `P0-05-versioned-visual-estimator`
- Objetivo: completar `strategy: estimator` y aplicar coste visual conservador por dimensiones.
- Probables archivos/componentes: `internal/adapter/tokencounter/tokencounter.go`, nuevos archivos internos si simplifican formulas, `tokencounter_test.go`, `internal/app/composition.go`, `model_builder.go`, `context_foundation_test.go`.
- Pasos:
1. Cambiar la factory para recibir strategy+ID sin crear un port nuevo.
2. Mantener `byte_bound` para `Media==0`; con media devolver error explicito de capacidad.
3. Implementar `estimator/visual-tile-conservative-v1` exactamente segun FR-14 y `Exact=false`.
4. Sumar `len(payload contable)` y todos los costes visuales usando aritmetica overflow-safe.
5. Rechazar MIME/dimensiones/detail/serializer invalidos y IDs desconocidos.
6. Propagar `CounterID` desde `ResolvedModel` en startup, modelos auxiliares y live checker.
7. Sustituir el test que hoy espera rechazo runtime de `official` por una matriz de disponibilidad coherente con loader/doctor.
- Dependencias: `P0-04`.
- Validacion: igualdad por dimensiones con distinta compresion, suma multi-image, low/auto/high, overflow, cancelacion, unknown ID y media con byte-bound.
- Condicion de completado: el total multimodal es payload sin binario + visual; ningun fallback cuenta base64 o asigna coste cero.

### Task P0-06

- ID: `P0-06-profile-validation-and-errors`
- Objetivo: impedir configuraciones que solo fallen con trafico y publicar errores de adjunto accionables.
- Probables archivos/componentes: `internal/agentdef/loader.go`, `loader_test.go`, `internal/app/cli_model.go`, `composition.go`, `internal/usecase/doctor/service.go`, `service_test.go`, `internal/usecase/bot/service.go`, bot tests.
- Pasos:
1. Validar que el perfil seleccionado por `attachment_analyzer` use una combinacion disponible y capaz de media; en P0, `estimator/visual-tile-conservative-v1`.
2. Hacer que doctor offline invoque la misma resolucion de disponibilidad que startup y muestre strategy+ID sin secretos.
3. Mantener doctor live por la misma factory; no hardcodear `byte_bound` para el analyzer.
4. Introducir errores typed o codigos internos para invalid, unsupported, dimensions y normalized-too-large.
5. Mapearlos a mensajes Slack concisos por filename; no publicar errores raw del decoder/counter ni sugerir reducir texto cuando el problema es la imagen.
6. Actualizar seeds/examples/documentacion de setup para exigir estimator en el perfil visual, sin inventar un proveedor desplegado.
- Dependencias: `P0-05`.
- Validacion: loader, startup, offline/live doctor y bot error tests cubren combinaciones conocidas/desconocidas y cero HTTP/model calls.
- Condicion de completado: una configuracion visual invalida falla antes de servir trafico y cada fallo de imagen tiene mensaje seguro y determinista.

### Task P0-07

- ID: `P0-07-adk-end-to-end-regression`
- Objetivo: probar la ruta productiva completa con budget de 65.536 y fixture realista.
- Probables archivos/componentes: nuevo `internal/integration/attachment_image_test.go` o suite de integracion existente mas cercana, fakes ADK/HTTP/Artifact.
- Pasos:
1. Construir processor, Artifact in-memory y modelo OpenAI-compatible con estimator real.
2. Hacer que el fake visual primero solicite `load_artifacts` y luego responda una descripcion textual.
3. Procesar el fixture P0-01 y verificar que el Artifact cargado es derivado canonico.
4. Capturar el segundo request: data URL real presente, payload contable sin base64, total <= 65.536 y exactamente una llamada visual HTTP admitida tras tool call.
5. Verificar descripcion `image-description`, root text-only y ausencia de bytes/base64 en eventos durables.
6. Agregar caso visual sobre presupuesto con cero requests HTTP.
- Dependencias: `P0-03` a `P0-06`.
- Validacion: test hermetico repetible con contadores de Artifact, model y HTTP.
- Condicion de completado: reproduce y corrige el incidente end-to-end sin credenciales live.

### Task P0-08

- ID: `P0-08-predecessor-trd-update`
- Objetivo: alinear documentacion autoritativa y evidencia final con el contrato implementado.
- Probables archivos/componentes: `docs/SLACK-FILES-ADK-ARTIFACTS-TRD.md`, este `TRD.md`, docs de provider/setup si contienen examples visuales.
- Pasos:
1. Cambiar en el TRD predecesor “raw downloaded file”/save directo por “derivado visual normalizado”; mantener original para texto/audio.
2. Documentar MIME/extension canonicos, limites, estrategia/ID y fallo sin fallback.
3. Registrar comandos y tests ejecutados por el worker, sin afirmar checks no realizados.
4. Verificar que docs no recomienden `byte_bound` para `attachment_analyzer`.
- Dependencias: `P0-07`.
- Validacion: busqueda de afirmaciones conflictivas y revision de trazabilidad FR/AC.
- Condicion de completado: documentacion antigua y nueva describen el mismo Artifact visual y guard multimodal.

## P1: capacidad operativa y hardening

### Task P1-01

- ID: `P1-01-image-detail-capability`
- Objetivo: hacer `image_detail` opt-in por perfil y emitirlo solo cuando se configure.
- Probables archivos/componentes: `internal/agentdef/types.go`, `loader.go`, `resolver.go`, tests, `internal/adapter/openaillm/options.go`, `convert.go`, `model_builder.go`.
- Pasos:
1. Agregar campo `image_detail` a profile/resolved model.
2. Validar solo `auto|low|high`, solo para `openai_compatible`; vacio sigue valido.
3. Propagar al adapter y fijar `detail` en `image_url` solo si no esta vacio.
4. Pasar detail efectivo al sidecar; omitido se estima conservadoramente como auto/high.
5. Probar body omitido/auto/low/high y rechazo de valores desconocidos.
- Dependencias: P0 completo.
- Validacion: loader/resolver/conversion/counter contract tests.
- Condicion de completado: default no cambia body; opt-in cambia body y estimacion de forma coherente.

### Task P1-02

- ID: `P1-02-configurable-max-edge`
- Objetivo: permitir endurecer edge final sin permitir ampliarlo por encima del limite de seguridad.
- Probables archivos/componentes: `internal/config/config.go`, `yaml.go`, `validate.go`, config tests, `internal/app/composition.go`, adkartifact constructor/options.
- Pasos:
1. Agregar `slack.files.image_max_edge_pixels`, default 1.568, rango 512..1.568.
2. Pasar valor al processor/normalizer mediante config concreta, sin port nuevo.
3. Recortar la secuencia de retries al max configurado y mantener no-upscale.
4. Actualizar defaults/examples y doctor offline.
- Dependencias: P0 completo.
- Validacion: config default/lower/bounds y normalizer con 512/1024/1568.
- Condicion de completado: operador puede reducir resolucion; ningun config supera limites P0.

### Task P1-03

- ID: `P1-03-doctor-metrics-logs`
- Objetivo: hacer observable normalizacion y estimation sin contenido sensible.
- Probables archivos/componentes: `internal/domain/context_metrics.go`, `internal/adapter/metrics/recorder.go`, tests, `internal/adapter/adkartifact`, `openaillm/llm.go`, `internal/usecase/doctor/service.go`, `internal/app/composition.go`.
- Pasos:
1. Agregar las metricas y labels bounded del TRD; dimensiones/bytes/tokens como values.
2. Inyectar el recorder existente al processor y modelos auxiliares.
3. Registrar source/final dimensions, bytes, format, attempts, strategy/ID, visual estimate y outcome.
4. Ampliar doctor para validar edge, estimator ID, media capability y coherencia detail/estimator.
5. Auditar logs y tests para demostrar ausencia de base64, EXIF, contenido y URLs.
- Dependencias: `P1-01`, `P1-02`.
- Validacion: metric snapshots/allowlist, doctor matrix y logger capture.
- Condicion de completado: operador distingue normalizacion, rechazo y coste estimado sin inspeccionar imagen.

### Task P1-04

- ID: `P1-04-image-decoder-fuzzing`
- Objetivo: endurecer parsers y aritmetica frente a entradas arbitrarias.
- Probables archivos/componentes: `internal/adapter/adkartifact/image_fuzz_test.go`, corpus `testdata/fuzz/`, dependency audit notes si existe convencion.
- Pasos:
1. Fuzzear sniff/DecodeConfig/normalizacion con corpus JPEG/PNG/WebP/GIF valido, truncado y corrupto.
2. Assert no panic, timeout no acotado, overflow, output > limites ni formato fuera de allowlist.
3. Agregar semillas EXIF extrema, GIF multi-frame, alpha y dimensiones declaradas hostiles.
4. Ejecutar fuzz con budget documentado en hardening; suite normal conserva seeds como regresion.
- Dependencias: `P1-03`.
- Validacion: `go test` normal y ejecucion fuzz documentada por el worker.
- Condicion de completado: findings se convierten en tests de regresion y no quedan crashes reproducibles.

## P2: precision y extensiones

### Task P2-01

- ID: `P2-01-provider-estimator-catalog`
- Objetivo: agregar formulas oficiales/versionadas por modelo sin cambiar semantica fail-closed.
- Probables archivos/componentes: `internal/adapter/tokencounter`, factory/composition, doctor, provider docs/tests.
- Pasos:
1. Seleccionar solo proveedores con documentacion publica estable de patches/tiles/detail.
2. Crear un ID versionado por formula y fijar fixtures de dimensiones/resultados documentados.
3. Marcar `Exact` segun contrato real; no confundir estimate con billing exacto.
4. Mantener conservative-v1 para proveedores sin formula, solo por seleccion explicita.
- Dependencias: metricas P1.
- Validacion: golden tables por ID y unknown model/ID fail-closed.
- Condicion de completado: cada ID tiene fuente, version, formula, tests y doctor support.

### Task P2-02

- ID: `P2-02-estimate-vs-usage-evaluation`
- Objetivo: medir deriva entre estimacion y usage real sin usar el modelo para routing.
- Probables archivos/componentes: `internal/adapter/openaillm` response usage parsing, metricas, tests con HTTP server, docs operativas.
- Pasos:
1. Capturar usage reportado cuando el proveedor lo ofrezca y separarlo de admission.
2. Emitir estimate, actual y ratio sin contenido ni IDs de usuario.
3. Alertar subestimacion; nunca ajustar formula automaticamente ni relajar un request en curso.
4. Versionar una formula corregida en vez de mutar semantica de un ID existente.
- Dependencias: `P2-01`.
- Validacion: respuestas con/sin usage, malformed usage y metric cardinality.
- Condicion de completado: deriva es medible y no afecta determinismo de admission.

### Task P2-03

- ID: `P2-03-gif-contact-sheet`
- Objetivo: reemplazar primer-frame por contact sheet bounded cuando aporte valor.
- Probables archivos/componentes: `internal/adapter/adkartifact/image.go`, tests GIF, config solo si se justifica.
- Pasos:
1. Definir max frames, muestreo temporal, grid, dimensiones y bytes dentro de limites P0.
2. Decodificar frames con limites acumulados de pixeles/memoria.
3. Renderizar contact sheet canonico PNG/JPEG y advertencia descriptiva.
4. Mantener primer-frame como fallback explicito ante animaciones fuera de limites.
- Dependencias: P1 fuzzing.
- Validacion: GIF corto/largo, disposal, alpha, corrupt frame y memory bounds.
- Condicion de completado: animacion aporta varios frames sin exceder ningun limite P0.

### Task P2-04

- ID: `P2-04-provider-files-api-evaluation`
- Objetivo: evaluar Files API por proveedor como transporte, no como bypass del guard.
- Probables archivos/componentes: documento de decision futuro; adapters solo tras aprobar contrato.
- Pasos:
1. Comparar soporte, auth, retencion, borrado, privacidad, idempotencia, limits y pricing contra data URL.
2. Exigir normalizacion y sidecar visual identicos; el transporte no cambia el coste de admission.
3. Definir lifecycle/recovery antes de persistir file IDs.
4. Rechazar una abstraccion generica si solo un proveedor la necesita.
- Dependencias: datos de P2-02.
- Validacion: decision documentada con threat model y prototipo hermetico; no live credentials en tests.
- Condicion de completado: adoptar o rechazar Files API con evidencia, sin alterar P0 por defecto.

## Matriz de trazabilidad

| Requisitos/criterios | Tasks |
| --- | --- |
| FR-01 a FR-10; EXIF/alpha/WebP/GIF/corrupcion/bombs | P0-01 a P0-03 |
| FR-11 a FR-17; payload sin base64; rechazo pre-HTTP | P0-04, P0-05, P0-07 |
| FR-18; startup/doctor/error messages | P0-06 |
| ADK load_artifacts -> request visual -> descripcion | P0-07 |
| TRD predecesor consistente | P0-08 |
| FR-19 image_detail | P1-01 |
| FR-20 max edge configurable | P1-02 |
| Observabilidad y doctor ampliado | P1-03 |
| Fuzzing/decoder hardening | P1-04 |
| Estimadores oficiales y usage real | P2-01, P2-02 |
| Contact sheet GIF y Files API | P2-03, P2-04 |

## Validacion global

Ejecutar tests enfocados despues de cada task y esta secuencia al cerrar cada prioridad:

```sh
gofmt -w <changed-go-files>
go test -count=1 ./internal/adapter/adkartifact ./internal/adapter/openaillm ./internal/adapter/tokencounter
go test -count=1 ./internal/agentdef ./internal/config ./internal/app ./internal/usecase/doctor ./internal/usecase/bot ./internal/integration
go test ./...
go vet ./...
go build -trimpath ./cmd/local-agent
git diff --check
```

La evidencia minima P0 debe incluir:

- size/dimensiones/formato del fixture y derivado;
- count legacy > 65.536 y count nuevo <= 65.536 para el caso aceptado;
- count nuevo > hard limit y cero HTTP para el caso rechazado;
- dos compresiones, mismas dimensiones/detail, mismo coste visual;
- request HTTP con data URL y payload contable sin base64;
- una llamada `load_artifacts` y descripcion textual final;
- regresion de texto, tools y audio.

## Rollout

1. Confirmar que P0 completo esta mergeado y los tres comandos obligatorios pasan.
2. Actualizar el perfil real del analyzer a `strategy: estimator`, `id: visual-tile-conservative-v1`.
3. Ejecutar `bin/local-agent doctor`; corregir cualquier unknown ID/capability antes de `run`.
4. Si esta autorizado, ejecutar `doctor --live` y confirmar tool call visual con imagen diagnostica normalizada.
5. Desplegar con defaults P0 y probar el fixture controlado o equivalente sin datos sensibles.
6. Observar outcomes y resource use; P1 agrega metricas detalladas.
7. Ante regresion, detener, restaurar binario y perfil compatibles juntos. No requiere rollback DB.

## Riesgos de ejecucion

| Riesgo | Control |
| --- | --- |
| Task P0 demasiado amplia | Separar Artifact, envelope/counter e integracion; no refactorizar adapters adyacentes. |
| Fixture aumenta mucho el repo | Mantener uno sanitizado 550-700 KiB y generar variantes en memoria. |
| Dependencia sin soporte Go/licencia | Revisar antes de pin; preferir stdlib + libreria minima pure Go. |
| Perfil desplegado desconocido | Hacer startup/doctor fail-closed y tratar YAML operacional como precondicion. |
| Test solo valida fake superficial | Capturar Artifact real, envelope contable y body HTTP en la misma integracion. |
| Formula cambia en sitio | IDs inmutables/versionados; nueva formula usa nuevo ID. |
| Error publica detalle de decoder | Codigos typed + mapa Slack; tests de redaccion y ausencia de bytes/base64. |

## Definition of Done

- Todos los criterios de aceptacion P0 de `TRD.md` tienen test o evidencia operacional explicita.
- Artifact visual contiene derivado normalizado; texto y audio siguen con conducta previa.
- Counter multimodal no recibe base64 y suma coste visual versionado.
- Unknown strategy/ID/serializer y media con `byte_bound` fallan antes de HTTP.
- Reproducer de aproximadamente 600 KiB pasa end-to-end bajo hard limit 65.536.
- Caso visual sobre budget prueba cero llamadas HTTP.
- TRD predecesor queda actualizado y no conserva afirmaciones conflictivas.
- `go test ./...`, `go vet ./...`, `go build -trimpath ./cmd/local-agent` y `git diff --check` pasan sin skips no explicados.
- Dependencias nuevas estan fijadas, revisadas y no introducen CGO.
- No quedan findings high/critical, secretos, base64 o contenido visual en logs/metricas.

## Handoff

- Listo para dev worker: si.
- Primera task: `P0-01-realistic-regression-fixture` seguida en el mismo incremento por `P0-02-image-normalizer-core` para no entregar una rama roja.
- Primer punto de control: fixture demuestra inflacion legacy y normalizador produce derivado <= 2 MiB/1.568 edge.
- Bloqueadores previos al rollout, no a la implementacion: identificar y migrar el perfil real de `attachment_analyzer`; confirmar soporte del proveedor para data URL.
- Regla de alcance: P0 no incluye image_detail configurable, metricas nuevas completas, formulas oficiales, contact sheet ni Files API.
