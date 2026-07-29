# TRD: Agent Builder — Agente Constructor de Agentes

## 1. Resumen Ejecutivo
Feature para crear definiciones de agentes (AgentDef YAML) desde Slack mediante
lenguaje natural o formulario modal, aprovechando el loader existente en agentdef.

## 2. Contexto y Justificación
- local-agent ya carga agentes desde .local-agent/agents/*.yaml durante startup
- El paquete agentdef tiene tipos, validación y resolución completos
- Permitir crear agentes desde Slack empodera a los usuarios sin editar YAML manual

## 3. Requisitos Funcionales
### RF1: preview_agent_def (tool read-only)
- Recibe name, description, instruction, model (opcional)
- Compila a AgentDef tipado (nunca parsea YAML del modelo)
- Valida contra validateDefinitions()
- Resuelve modelo vía ResolveModel()
- Retorna YAML canónico + SHA-256
- No escribe nada

### RF2: install_agent_def (tool con confirmación)
- Recibe name + definition_hash del preview
- Revalida TOCTOU, nombre y hash
- Usa flujo existente de PendingConfirmation → Approve/Reject
- Escribe únicamente el YAML a .local-agent/agents/<name>.yaml
- Create-no-replace, fsync, anti-symlink
- No modifica root_agent ni ningún manifest
- La activación ocurre en el próximo reinicio, cuando agentdef.Load() descubre el YAML nuevo

### RF3: Draft Store
- Tabla agent_drafts en SQLite
- Campos: draft_id, team_id, actor_id, conversation_key, name, description, instruction, model, definition_hash, catalog_revision, status, created_at, expires_at
- Estados: draft → previewed → install_requested → installed|cancelled|expired|failed
- v1 usa una allowlist fija de tools read-only: list_messages, list_repos, list_directory, read_file, list_worktrees

### RF4: Modal Slack (1 vista)
- Botón "Crear agente" como entry point
- Campos: Nombre, Descripción, Instrucción, Modelo (dropdown)
- view_submission → persiste draft → cierra modal → publica preview en conversación
- Botón "Solicitar instalación" en el preview
- Sin preview inline, sin instalación directa desde modal

### RF5: Fallback por texto
- El usuario puede describir el agente conversacionalmente
- Mismo DTO y servicio de preview que el modal

## 4. Restricciones v1
- Solo LlmAgent leaf (no root_agent)
- tool_scope: invocation_scoped
- include_contents: none
- Providers allowlisted (solo openai_compatible)
- Sin AcpAgent, agent_cli, global_instruction, agent_tools, durable_session, roles
- Sin hot activation (se activa tras reinicio)
- Sin preview inline en modal
- Sin selectores externos dinámicos
- Colisiones de nombres con tools existentes (list_messages, create_canvas, etc.) se rechazan en preview/startup
- Orden determinista: ordenar los agentes por nombre

## 5. Arquitectura

### 5.1 Componentes nuevos
| Componente | Tipo | Responsabilidad |
|-----------|------|----------------|
| AgentBuilderService | use case | Compilar Draft → AgentDef, validar, canonicalizar |
| AgentWriter | adapter | Escritura atómica y segura a StateDir/agents/ |
| AgentDraftStore | adapter | Persistencia de drafts en SQLite |
| BuilderModalPresenter | adapter | Renderizar vista Block Kit del modal |
| BuilderSubmissionHandler | adapter | Procesar view_submission |

### 5.2 Componentes a modificar
| Componente | Cambio |
|-----------|--------|
| internal/agentdef/loader.go | Exportar ValidateDefinitions, agregar ValidateCandidateAgent |
| internal/agentdef/types.go | Agregar validación de nombres, reserved words, límites |
| internal/adapter/toolfactory/toolfactory.go | Agregar previewAgentDefTool e installAgentDefTool |
| internal/adapter/slack/listener.go | Extender para view_submission y botón builder |
| internal/app/composition.go | Cablear nuevas dependencias |

### 5.3 Flujo texto
```
Usuario describe agente
  → root_agent llama preview_agent_def
  → preview_agent_def compila, valida, retorna YAML + SHA-256
  → usuario ve preview y decide
  → usuario pide instalar
  → root_agent llama install_agent_def (name + definition_hash)
  → PendingConfirmation → botones Approve/Reject
  → Approve → install_agent_def revalida y escribe únicamente YAML con create-no-replace + fsync
  → No modifica root_agent ni ningún manifest
  → agentdef.Load() descubre YAML durante el próximo reinicio y activa agente
```

### 5.4 Flujo modal
```
Usuario hace clic "Crear agente"
  → Se abre modal con campos
  → Usuario completa y envía
  → view_submission → valida → persiste draft → ACK (cierra modal)
  → Asíncrono: compila, valida, publica preview en conversación
  → Preview incluye botón "Solicitar instalación"
  → Usuario hace clic → install_agent_def recibe name + definition_hash (mismo flujo que texto)
  → Approve → escribe únicamente YAML; activación implícita durante el próximo reinicio mediante agentdef.Load()
```

### 5.5 Auto-discovery durante startup
Durante startup, `prepareRootAgentTools()` itera todo `defs.Agents`, excluye `root_agent`, conserva solo definiciones con `Role != ""` y solo tipos `LlmAgent` o `AcpAgent`. Los YAML nuevos quedan disponibles sin modificar `root_agent` ni un manifest.

## 6. Seguridad
- Allowlist administrativa específica para crear agentes
- Rechazar nombres reservados (root_agent, semillas) y colisiones
- Límites: description 500 chars, instruction 3.000 chars
- Path derivado del nombre validado (nunca del usuario como texto)
- create-no-replace, fsync, anti-symlink
- private_metadata solo para draft_id opaco
- Revalidar autorización en cada paso
- Rate limit y cuota máxima

### 6.1 Allowlist read-only para children
- Los agents children solo reciben la allowlist positiva fija de tools read-only: list_messages, list_repos, list_directory, read_file, list_worktrees
- Tools mutables como install_agent_def, create_canvas y export_* no deben pasar a agents children
- La selección usa allowlist positiva, no exclusión puntual

## 7. Plan de Implementación

### Fase 0: Fundación (agentdef APIs)
- Exportar ValidateDefinitions
- Agregar ValidateCandidateAgent
- Agregar validación de nombres (patrón, reserved, seeds, colisiones)
- Agregar límites de tamaño

### Fase 1: Preview tool
- DTO AgentDraft (domain/agentdraft.go)
- AgentBuilderService (usecase/agentbuilder/service.go)
- previewAgentDefTool en toolfactory
- Tests

### Fase 2: Install tool
- AgentWriter (adapter/filesystem/agentwriter.go)
- installAgentDefTool con RequireConfirmationProvider
- Integración con flujo de confirmación existente
- install_agent_def solo escribe el YAML con create-no-replace + fsync; no modifica root_agent ni manifest
- La activación queda implícita para el próximo reinicio, cuando agentdef.Load() descubre el nuevo YAML

### Fase 3: Draft Store
- Migración SQLite (agent_drafts)
- AgentDraftStore (adapter/db/draftstore.go)
- Interfaz AgentDraftStore (port/draftstore.go)

### Fase 4: Modal
- BuilderModalPresenter (adapter/slack/builder_modal.go)
- BuilderSubmissionHandler (adapter/slack/builder_handler.go)
- Extensión de listener.go para view_submission
- Botón "Crear agente" + preview message con "Solicitar instalación"

### Fase 5: Integración
- Cablear en composition.go
- Redactor de secretos en drafts
- Actualizar doctor para inspeccionar agentes auto-descubiertos
- Tests integrales
- QA manual en Slack desktop y mobile

## 8. Dependencias
- Ninguna externa nueva (usa functiontool.New, ADK runtime, sqlite existentes)

## 9. Criterios de Aceptación
- [ ] preview_agent_def retorna YAML canónico + SHA-256 para input válido
- [ ] preview_agent_def rechaza nombres inválidos, reservados, colisiones
- [ ] preview_agent_def rechaza providers no allowlisted
- [ ] install_agent_def escribe atómicamente con fsync
- [ ] install_agent_def rechaza hash incorrecto
- [ ] Modal se abre con trigger_id y campos correctos
- [ ] view_submission persiste draft y publica preview
- [ ] Submission inválido conserva el modal abierto con errores por campo
- [ ] ACK de interacciones ocurre dentro del límite de 3 segundos
- [ ] Actor, equipo y conversación incorrectos rechazan toda operación
- [ ] Flujo de confirmación Approve/Reject funciona con install
- [ ] Fallback por texto funciona idéntico al modal
- [ ] Drafts expiran y no permiten instalación vencida
- [ ] Draft vencido, cancelado o ya instalado falla determinísticamente
- [ ] Dos instalaciones concurrentes del mismo nombre producen exactamente un ganador
- [ ] Symlink, hardlink o archivo preexistente no puede reemplazarse
- [ ] Instrucciones de 3.001 caracteres son rechazadas coherentemente
- [ ] Migración desde schema v19 está probada (SchemaVersion 19)
- [ ] Ningún contenido bruto de instrucciones aparece en logs o errores
