# TRD: Agent Builder — Agente Constructor de Agentes

## 1. Resumen Ejecutivo

Feature para crear definiciones de agentes (`AgentDef` YAML) desde Slack mediante
lenguaje natural o formulario modal, aprovechando el loader existente en
`agentdef`. La versión 2 agrega soporte explícito para `LlmAgent` y `AcpAgent`,
con validación cruzada entre tipo de agente y proveedor.

El YAML canónico generado durante el preview es la fuente de verdad del draft y
de la instalación. El proceso de instalación no vuelve a construir la
definición a partir de campos sueltos.

## 2. Contexto y Justificación

- local-agent carga agentes desde `.local-agent/agents/*.yaml` durante startup.
- El paquete `agentdef` tiene tipos, validación y resolución de proveedores.
- `AcpAgent` usa `runtime: provider/profile`, no `model`, y delega la ejecución
  en un proveedor ACP.
- Permitir crear agentes desde Slack evita editar YAML manualmente, sin permitir
  seleccionar proveedores o políticas fuera de la configuración administrativa.

## 3. Contrato del Draft v2

### 3.1 Tipos y campos

El DTO de dominio debe incorporar estos campos al contrato actual:

```go
type AgentKind string

const (
	AgentKindLLM AgentKind = "llm"
	AgentKindACP AgentKind = "acp"
)

type AgentDraft struct {
	Name            string
	Description     string
	Instruction     string
	Kind            AgentKind
	ProviderProfile string // referencia canónica provider/profile
	Model           string // opcional; vacío/null para ACP
	ExecutionMode   string
	TimeoutSeconds  int
}
```

Reglas del contrato:

- `Kind` acepta únicamente `llm` y `acp`; si se omite, vale `llm`.
- `ProviderProfile` identifica un perfil ya cargado, con formato
  `provider/profile`.
- Para `llm`, `ProviderProfile` se convierte en `AgentDef.model` y el proveedor
  debe ser `openai_compatible`.
- Para `acp`, `ProviderProfile` se convierte en `AgentDef.runtime` y el
  proveedor debe ser ACP con nombre `opencode`. `AgentDef.model` queda ausente,
  vacío o `NULL`; no se inventa un modelo LLM.
- `ExecutionMode` acepta `foreground` para ambos tipos y `durable_job` solo
  para `acp`. El valor por defecto es `foreground`.
- `TimeoutSeconds` aplica solo a ACP. Su valor por defecto es `7200` segundos y
  no puede superar `86400` segundos (`agentdef.MaxACPTimeoutSeconds`).
- En ACP, `confirmation: required` se genera siempre y no es configurable por
  el usuario.

### 3.2 Validación cruzada

El use case valida el draft contra el catálogo actual antes de generar YAML:

| `Kind` | `AgentClass` generado | Referencia | Proveedor permitido |
|--------|------------------------|------------|---------------------|
| `llm` | `LlmAgent` | `model: provider/profile` | `openai_compatible` |
| `acp` | `AcpAgent` | `runtime: provider/profile` | ACP `opencode` únicamente |

Se rechazan combinaciones cruzadas, perfiles inexistentes, proveedores no
preconfigurados, referencias mal formadas y campos incompatibles. El nombre,
descripción e instrucción conservan los límites y validaciones de `agentdef`.

## 4. Requisitos Funcionales

### RF1: `preview_agent_def` (tool read-only)

- Recibe `name`, `description`, `instruction`, `kind`, `provider_profile`,
  `execution_mode` y `timeout_seconds`.
- Mantiene `kind: llm` como default para clientes que no envíen el campo.
- Para `llm`, resuelve un perfil `openai_compatible`; si el perfil no se
  especifica, puede usar el default determinista existente de ese catálogo.
- Para `acp`, exige una referencia válida a un perfil del proveedor `opencode`.
- Normaliza `execution_mode` a `foreground` cuando no se especifica.
- Normaliza el timeout ACP a `7200` segundos cuando no se especifica y rechaza
  valores menores que cero o mayores que `86400`. Los campos de timeout no son
  válidos para `llm`.
- Compila en el use case, nunca parsea YAML generado por el modelo:
  - `LlmAgent`: `agent_class`, `name`, `model`, `description`, `instruction`,
    `include_contents: none` y `tool_scope: invocation_scoped`.
  - `AcpAgent`: `agent_class`, `name`, `runtime`, `description`, `instruction`,
    `execution_mode`, `timeout_seconds` y `confirmation: required`. `model`,
    `tool_scope`, `include_contents` y campos de sesión ADK no se generan.
- Valida el candidato con las reglas de `agentdef`, incluyendo la relación
  `kind`/`provider` y la allowlist ACP.
- Canonicaliza el YAML y devuelve YAML canónico más SHA-256.
- Persiste el draft junto con el YAML canónico, su hash y los metadatos de
  ejecución. No escribe un archivo de agente.

### RF2: `install_agent_def` (tool con confirmación)

- Recibe `draft_id` y, opcionalmente, `name` y `definition_hash` para la
  comprobación de identidad del preview.
- Revalida actor, equipo, conversación, estado, expiración, autorización,
  nombre y hash antes de instalar.
- Carga `canonical_yaml` persistido en `agent_drafts`.
- Recalcula SHA-256 sobre los bytes canónicos persistidos y lo compara con el
  hash del draft y con el hash solicitado.
- Decodifica el YAML persistido a `AgentDef` y lo revalida contra el catálogo
  actual, incluyendo tipo, proveedor, runtime, modo, timeout y
  `confirmation: required`.
- Escribe exactamente esos bytes canónicos con `create-no-replace`, `fsync` y
  protección anti-symlink.
- No reconstruye un `LlmAgent` o `AcpAgent` a partir de `name`, `model` u otros
  campos de la tabla.
- Un draft legado sin `canonical_yaml` no se puede instalar y falla cerrado; no
  se regenera YAML durante la instalación.
- Usa el flujo existente de `PendingConfirmation` → Approve/Reject.
- No modifica `root_agent` ni ningún manifest. La activación ocurre en el
  próximo reinicio, cuando `agentdef.Load()` descubre el YAML.

### RF3: Draft Store y migración de schema v22

La tabla `agent_drafts` conserva el ciclo de vida actual y agrega el contrato
v2. La migración desde schema v19 hasta schema v22 debe ser transaccional y
probada.

Campos relevantes de `agent_drafts` en v22:

| Campo | Tipo | Regla |
|-------|------|-------|
| `draft_id` | `TEXT` | PK opaca |
| `team_id`, `actor_id`, `conversation_key` | `TEXT` | ownership obligatorio |
| `name`, `description`, `instruction` | `TEXT` | contenido validado |
| `model` | `TEXT NULL` | referencia/modelo LLM; NULL para ACP |
| `kind` | `TEXT NOT NULL` | `llm` o `acp` |
| `execution_mode` | `TEXT NOT NULL` | `foreground` o `durable_job` según tipo |
| `timeout_seconds` | `INTEGER NOT NULL` | `0` para no aplicable; ACP hasta `86400` |
| `canonical_yaml` | `TEXT NOT NULL` para drafts v2 | bytes/texto canónico persistido |
| `definition_hash` | `TEXT NOT NULL` | SHA-256 de `canonical_yaml` |
| `catalog_revision`, `status`, timestamps | existentes | ciclo de vida y TOCTOU |

Detalles de migración:

- La base v20 tenía `model TEXT NOT NULL` y no tenía `kind`,
  `execution_mode`, `timeout_seconds` ni `canonical_yaml`.
- v22 agrega esas columnas, cambia `model` a nullable mediante reconstrucción
  segura de la tabla si el motor lo requiere y conserva índices, checks y
  estados existentes.
- Los defaults de compatibilidad para filas antiguas son `kind=llm`,
  `execution_mode=foreground`, `timeout_seconds=0` y `canonical_yaml=''`.
  Esas filas no son instalables hasta tener un YAML canónico v2; no se debe
  fabricarlo a partir de columnas históricas.
- Si v22 ya contiene otras migraciones, los cambios de `agent_drafts` se
  integran en la migración v22 existente, sin crear una versión paralela.
- El store debe leer y escribir todos los campos nuevos y usar representación
  nullable al escanear `model`.

Estados: `draft` → `previewed` → `install_requested` →
`installed|cancelled|expired|failed`.

### RF4: Modal Slack

- El modal tiene campos Nombre, Descripción, Instrucción y proveedor/perfil.
- Incluye selector **Tipo** con `LLM` y `ACP`; `LLM` es el default.
- El dropdown de proveedor/perfil se filtra por tipo:
  - LLM: solo perfiles de proveedores `openai_compatible`.
  - ACP: solo perfiles del proveedor ACP allowlisted `opencode`.
- Para ACP muestra selector **Ejecución** con `foreground` y `durable_job`.
  Para LLM no muestra `durable_job` y envía solo el modo compatible.
- Para ACP muestra **Timeout**, con default `7200` segundos y máximo `86400`.
  Para LLM el campo no se muestra ni se persiste como política de timeout.
- `view_submission` valida y persiste el draft, cierra el modal y publica el
  preview en la conversación.
- El preview contiene el botón **Solicitar instalación** y usa el mismo flujo
  de confirmación que el fallback por texto.
- No hay preview inline ni instalación directa desde el modal.

### RF5: Fallback por texto

- El usuario puede describir el agente conversacionalmente.
- El root agent debe producir el mismo `AgentDraft` v2 que el modal.
- Preview, validación, persistencia, hash e instalación son idénticos en ambos
  canales.

## 5. Restricciones v2

- Se soportan `LlmAgent` y `AcpAgent` como agentes leaf; no `root_agent`.
- `LlmAgent` solo puede usar `openai_compatible`.
- `AcpAgent` solo puede usar el proveedor ACP preconfigurado y allowlisted
  `opencode`; no se aceptan otros proveedores ACP, `agent_cli` ni HTTP fields.
- `AcpAgent` requiere `runtime: opencode/profile`,
  `confirmation: required` y no usa `model`.
- `foreground` está disponible para ambos tipos; `durable_job` solo para ACP.
- Timeout ACP default `7200` segundos, máximo `86400` segundos.
- `tool_scope: invocation_scoped` e `include_contents: none` aplican al
  `LlmAgent`; no se copian a `AcpAgent`.
- No se permiten `global_instruction`, `agent_tools`, `durable_session` ni
  `roles` en los agentes creados.
- No hay hot activation; el agente queda disponible tras el siguiente reinicio.
- Colisiones de nombre con agentes reservados o tools directos se rechazan.
- Orden determinista para defaults y opciones de dropdown.

## 6. Arquitectura

### 6.1 Responsabilidades

| Componente | Responsabilidad |
|-----------|----------------|
| `AgentBuilderService` (use case) | Compilar por `Kind`, resolver perfiles, normalizar ejecución/timeout, validar, canonicalizar y calcular hash |
| `AgentWriter` (adapter) | Escritura atómica y segura de bytes ya validados |
| `AgentDraftStore` (adapter) | Persistencia de drafts v2 y estados en SQLite |
| `BuilderModalPresenter` (adapter) | Renderizar controles y opciones filtradas |
| `BuilderSubmissionHandler` (adapter) | Convertir submission a draft y delegar en el use case |
| `toolfactory` (adapter) | Adaptar argumentos ADK, invocar puertos y publicar resultados |

La construcción de `AgentDef` pertenece al use case. `toolfactory` no debe
reconstruir manualmente un `LlmAgent`, seleccionar defaults, generar YAML ni
duplicar la validación del builder. El adapter de tool solo entrega el draft al
servicio y el `draft_id` al servicio/puerto de instalación.

### 6.2 Cambios por paquete

| Paquete | Cambio |
|---------|--------|
| `internal/domain/agentdraft.go` | Agregar `AgentKind`, `ProviderProfile`, `ExecutionMode`, `TimeoutSeconds` y modelo nullable para ACP |
| `internal/port/agentbuilder.go` | Agregar `ExecutionMode string` y `TimeoutSec int` a `AgentDefPreview` |
| `internal/usecase/agentbuilder` | Compilar LLM/ACP, validar combinaciones, canonicalizar y exponer la fuente canónica para install |
| `internal/adapter/toolfactory` | Eliminar reconstrucción manual; mapear inputs/outputs y delegar |
| `internal/adapter/sqlite` | Migración v22 y store para campos nuevos/canonical YAML |
| `internal/adapter/slack` | Modal con tipo, filtro de perfiles, ejecución y timeout ACP |
| `internal/app/composition.go` | Pasar perfiles preconfigurados de ambos tipos al presenter; no filtrar solo `openai_compatible` |
| `internal/agentdef` | Reusar validación existente de `AcpAgent`, `runtime`, modo, timeout y confirmación |

### 6.3 Flujo de preview

```text
Usuario describe agente o completa modal
  → se forma AgentDraft v2
  → AgentBuilderService valida kind + provider/profile
  → compila LlmAgent o AcpAgent
  → canonicaliza YAML + SHA-256
  → persiste draft v2 y canonical_yaml
  → publica preview con clase, modo, timeout y hash
```

### 6.4 Flujo de instalación

```text
Usuario solicita instalación
  → PendingConfirmation → Approve/Reject
  → carga draft por actor + conversación
  → lee canonical_yaml persistido
  → recalcula hash
  → decodifica AgentDef
  → revalida contra catálogo y restricciones actuales
  → escribe exactamente YAML canónico con create-no-replace + fsync
  → activa en próximo reinicio mediante agentdef.Load()
```

### 6.5 Auto-discovery durante startup

Durante startup, `prepareRootAgentTools()` itera `defs.Agents`, excluye
`root_agent` y excluye definiciones con `Role != ""`. Conserva definiciones
`LlmAgent` y `AcpAgent`, ordenadas por nombre. El auto-discovery ya soporta
`AcpAgent`; el Agent Builder no debe agregar un camino paralelo ni cambiar ese
filtro.

## 7. Seguridad

- Solo se pueden seleccionar proveedores y perfiles preconfigurados en el
  catálogo cargado durante startup.
- La allowlist ACP contiene únicamente el proveedor `opencode`; el nombre no
  puede ser sustituido por una entrada enviada por el modelo o por el modal.
- `kind` y proveedor se validan juntos en preview, al persistir y nuevamente en
  install para cubrir TOCTOU y cambios del catálogo.
- `confirmation: required` es una política fija para todo `AcpAgent`; no se
  expone selector de confirmación.
- No se acepta `allow_always`. Las opciones ACP de permisos siguen siendo
  invocation-scoped y no crean política durable oculta.
- `timeout_seconds` ACP se normaliza al default `7200` y se rechaza sobre
  `86400`; no se permite desactivar el límite desde el draft.
- El hash se calcula sobre el YAML canónico exacto y se verifica antes de
  escribir.
- Actor, equipo, conversación, estado y expiración se revalidan en cada paso.
- Nombres reservados, colisiones, symlinks, hardlinks y archivos preexistentes
  no pueden sobrescribirse.
- Descripción, instrucción, YAML y credenciales no aparecen en logs ni errores
  sin redacción; `private_metadata` contiene solo un `draft_id` opaco.
- Se mantienen rate limit, cuota máxima y la allowlist positiva de tools
  read-only para children: `list_messages`, `list_repos`, `list_directory`,
  `read_file`, `list_worktrees`.

## 8. Plan de Implementación

### Fase 0: Contrato y validación

- Actualizar `AgentDraft` y `AgentDefPreview`.
- Definir `AgentKind` y defaults de modo/timeout.
- Cubrir validación cruzada kind/provider y restricciones ACP.

### Fase 1: Preview en use case

- Mover toda la construcción de `AgentDef` al `AgentBuilderService`.
- Generar `LlmAgent` o `AcpAgent` según `Kind`.
- Persistir YAML canónico, hash y metadatos v2.
- Añadir tests de preview para ambos tipos.

### Fase 2: Install desde YAML canónico

- Añadir lectura de `canonical_yaml` al draft store.
- Implementar hash, decode y revalidación del YAML persistido.
- Eliminar la reconstrucción manual en `installAgentDefTool`.
- Mantener confirmación, escritura atómica y activación al reinicio.

### Fase 3: Draft Store y schema v22

- Implementar migración v19 → v22 para `agent_drafts`.
- Añadir columnas, nullable de `model`, defaults de compatibilidad y checks.
- Probar filas v20/v21 y rechazo de drafts sin YAML canónico.

### Fase 4: Modal

- Añadir selector Tipo.
- Filtrar perfiles por tipo y permitir solo `opencode` para ACP.
- Mostrar Ejecución y Timeout únicamente para ACP.
- Validar errores por campo y preservar el modal ante submission inválido.

### Fase 5: Integración y QA

- Cablear composición con perfiles LLM y ACP.
- Corregir el filtro del modal que hoy solo contempla `openai_compatible`.
- Verificar auto-discovery sin alterar el soporte existente de `AcpAgent`.
- Ejecutar tests unitarios, integración SQLite y QA manual en Slack desktop y
  mobile.

## 9. Dependencias

- Ninguna dependencia externa nueva.
- Se reutilizan `agentdef`, ADK runtime, SQLite, el flujo de confirmación y el
  transporte Slack existentes.

## 10. Criterios de Aceptación

- [ ] `kind` ausente conserva compatibilidad y genera `LlmAgent`.
- [ ] `kind=llm` solo acepta perfiles `openai_compatible`.
- [ ] `kind=acp` genera `AcpAgent` con `runtime: opencode/profile`.
- [ ] Proveedores ACP distintos de `opencode`, perfiles inexistentes y
  combinaciones kind/provider inválidas son rechazados.
- [ ] ACP genera siempre `confirmation: required` y nunca `allow_always`.
- [ ] ACP acepta `foreground` y `durable_job`; LLM rechaza `durable_job`.
- [ ] Timeout ACP por defecto es `7200` segundos y `86401` se rechaza.
- [ ] Timeout y `durable_job` no aparecen como controles aplicables a LLM.
- [ ] `model` puede ser NULL para ACP y el YAML ACP usa `runtime`, no `model`.
- [ ] `AgentDefPreview` expone `ExecutionMode` y `TimeoutSec` correctos.
- [ ] Preview devuelve YAML canónico y SHA-256 reproducible para el mismo
  input y catálogo.
- [ ] El draft persiste `kind`, `execution_mode`, `timeout_seconds`, hash y
  `canonical_yaml`.
- [ ] Install carga el YAML canónico persistido, recalcula hash, decodifica,
  revalida y escribe esos mismos bytes.
- [ ] Install no reconstruye una definición desde columnas sueltas y rechaza
  un draft sin YAML canónico.
- [ ] La migración desde schema v19 hasta v22 conserva datos, índices y
  estados, y cambia `model` a nullable.
- [ ] Se prueban migraciones desde v19, v20 y v21, incluida la coexistencia con
  las demás migraciones v22.
- [ ] El modal muestra Tipo LLM|ACP y filtra correctamente perfiles por tipo.
- [ ] El modal solo muestra Ejecución y Timeout para ACP.
- [ ] Submission inválido conserva el modal abierto con errores por campo.
- [ ] El filtro de auto-discovery excluye `Role != ""` y conserva `AcpAgent`.
- [ ] Actor, equipo y conversación incorrectos rechazan toda operación.
- [ ] El flujo de confirmación Approve/Reject funciona para instalación ACP y
  LLM.
- [ ] Drafts expirados, cancelados o instalados fallan determinísticamente.
- [ ] Dos instalaciones concurrentes del mismo nombre producen exactamente un
  ganador.
- [ ] Symlink, hardlink o archivo preexistente no puede reemplazarse.
- [ ] Límites de nombre, descripción e instrucción se aplican con conteo de
  caracteres Unicode.
- [ ] Ningún contenido bruto de instrucciones, YAML o secretos aparece en
  logs o errores.

## 11. Correcciones Incorporadas

- El alcance de persistencia queda documentado como migración schema v19 → v22,
  no como una migración aislada de v20.
- Se corrige el filtrado de `Role`: se excluyen agentes con `Role != ""`, no se
  conservan.
- Se documenta que el auto-discovery ya soporta `AcpAgent`; el cambio requerido
  es no romperlo al integrar el Builder.
- La construcción de definiciones se ubica en el use case y no en
  `toolfactory`, evitando lógica duplicada y discrepancias entre preview e
  install.
