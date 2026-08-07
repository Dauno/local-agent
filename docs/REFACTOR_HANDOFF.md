---
# Refactor Handoff — slack-local-agent

Estado del refactor de sobre-ingeniería/duplicación por batches. Este documento es el punto de continuidad entre sesiones.

## Estado actual

- 9 batches de refactor completados + docs update, 9 commits, neto acumulado **−140 líneas**.
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
---
