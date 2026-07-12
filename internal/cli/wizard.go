package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/manifest"
	"github.com/Dauno/slack-local-agent/internal/secure"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
)

const privacyNotice = `Aviso de privacidad:
- Los mensajes recientes de conversaciones Slack autorizadas se almacenan localmente en SQLite.
- El contenido enviado al bot se transmite a Slack y al endpoint de modelo configurado al generar respuestas.
- Si un usuario autorizado invoca el bot en un canal, la respuesta del thread es visible para quienes puedan acceder a ese canal o thread, incluso si no están autorizados para invocar el bot.
`

const contextPrivacyNotice = `Contexto Slack opcional:
- Se enviarán al endpoint de modelo configurado el nombre, cargo, zona horaria y locale del usuario que invoca, además del nombre, topic y purpose del canal.
- Estos datos no se almacenan localmente por esta función. Las respuestas en canales siguen siendo visibles para quienes accedan a la conversación.
- Habilitarlo requiere reinstalar la app Slack para conceder users:read.
`

func runWizard(ctx context.Context, backend Backend, prompt *Prompter, output io.Writer) error {
	fmt.Fprintln(output, "[1/9] Creando artefactos locales faltantes")
	snapshot, existingSecrets, err := backend.PrepareSetup(ctx)
	if err != nil {
		return fmt.Errorf("initialize local artifacts: %w", err)
	}
	cfg := snapshot.Config

	fmt.Fprintln(output, "\n[2/9] Identidad del agente y de la app Slack")
	agentName, err := prompt.Text("Nombre del agente", cfg.Agent.Name, true)
	if err != nil {
		return err
	}
	appName, err := prompt.Text("Nombre de la app Slack", cfg.Slack.AppName, true)
	if err != nil {
		return err
	}
	botName, err := prompt.Text("Nombre visible del bot", cfg.Slack.BotDisplayName, true)
	if err != nil {
		return err
	}
	identity := bootstrap.Identity{AgentName: agentName, SlackAppName: appName, SlackBotDisplayName: botName}

	fmt.Fprintln(output, "\n[3/9] Crear la app Slack desde el manifest")
	creationURL, err := manifest.RenderCreationURL(manifest.Identity{AppName: appName, BotDisplayName: botName})
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Abre esta URL para crear la app con el manifest generado:\n%s\n", creationURL)
	fmt.Fprintln(output, "Manifest: https://api.slack.com/reference/manifests")
	fmt.Fprintln(output, "Socket Mode: https://api.slack.com/apis/connections/socket")
	fmt.Fprintln(output, "Tokens: https://api.slack.com/authentication/token-types#app")
	fmt.Fprintln(output, "El manifest configura Socket Mode, eventos y scopes. El token xapp- con connections:write se crea manualmente.")

	fmt.Fprintln(output, "\n[4/9] Instalar la app y configurar el token del bot")
	fmt.Fprintln(output, "En OAuth & Permissions, instala o reinstala la app y copia el Bot User OAuth Token.")
	botToken, err := prompt.Secret("SLACK_BOT_TOKEN", existingSecrets.SlackBotToken, "xoxb-")
	if err != nil {
		return err
	}

	fmt.Fprintln(output, "\n[5/9] Crear el token de Socket Mode")
	fmt.Fprintln(output, "En Basic Information, crea un app-level token con connections:write.")
	appToken, err := prompt.Secret("SLACK_APP_TOKEN", existingSecrets.SlackAppToken, "xapp-")
	if err != nil {
		return err
	}

	fmt.Fprintln(output, "\n[6/9] Restringir quién puede usar el bot")
	fmt.Fprintln(output, "Recomendación: comienza con tu Slack user ID. Para encontrarlo, abre tu perfil, More y Copy member ID.")
	var access bootstrap.AccessControl
	for {
		access.AllowedUserIDs, err = prompt.CSV("Slack user IDs permitidos (separados por coma)", cfg.Slack.AllowedUserIDs)
		if err != nil {
			return err
		}
		access.AllowAllUsers, err = prompt.Confirm("Permitir a todos los usuarios", cfg.Slack.AllowAllUsers)
		if err != nil {
			return err
		}
		access.AllowedTeamIDs, err = prompt.CSV("Team IDs permitidos opcionales", cfg.Slack.AllowedTeamIDs)
		if err != nil {
			return err
		}
		access.AllowedChannelIDs, err = prompt.CSV("Channel IDs permitidos opcionales", cfg.Slack.AllowedChannelIDs)
		if err != nil {
			return err
		}
		fmt.Fprint(output, contextPrivacyNotice)
		access.ContextEnabled, err = prompt.Confirm("Habilitar enriquecimiento de contexto Slack", cfg.Slack.Context.Enabled)
		if err != nil {
			return err
		}
		candidate := cfg
		candidate.Agent.Name = agentName
		candidate.Slack.AppName = appName
		candidate.Slack.BotDisplayName = botName
		candidate.Slack.AllowAllUsers = access.AllowAllUsers
		candidate.Slack.AllowedUserIDs = access.AllowedUserIDs
		candidate.Slack.AllowedTeamIDs = access.AllowedTeamIDs
		candidate.Slack.AllowedChannelIDs = access.AllowedChannelIDs
		candidate.Slack.Context.Enabled = access.ContextEnabled
		if err := config.Validate(candidate); err != nil {
			fmt.Fprintf(output, "Configuración inválida: %v\nVuelve a ingresar el control de acceso.\n", err)
			continue
		}
		break
	}

	fmt.Fprintln(output, "\n[7/9] Configurar la clave del modelo")
	modelKey, err := prompt.Secret(cfg.Model.APIKeyEnv, existingSecrets.ModelAPIKey, "")
	if err != nil {
		return err
	}
	secrets := bootstrap.Secrets{ModelAPIKey: modelKey, SlackBotToken: botToken, SlackAppToken: appToken}

	fmt.Fprintln(output, "\n[8/9] Confirmar cambios")
	fmt.Fprintf(output, "Agente: %s\nApp Slack: %s\nBot visible: %s\n", agentName, appName, botName)
	fmt.Fprintf(output, "Permitir todos: %t\nUsuarios: %s\nTeams: %s\nCanales: %s\nContexto Slack: %t\n",
		access.AllowAllUsers, displayList(access.AllowedUserIDs), displayList(access.AllowedTeamIDs), displayList(access.AllowedChannelIDs), access.ContextEnabled)
	if access.ContextEnabled {
		fmt.Fprintln(output, "Reinstala la app Slack para conceder users:read antes de ejecutar local-agent run.")
	}
	fmt.Fprintf(output, "%s: %s\nSLACK_BOT_TOKEN: %s\nSLACK_APP_TOKEN: %s\n",
		cfg.Model.APIKeyEnv, secure.Mask(modelKey), secure.Mask(botToken), secure.Mask(appToken))
	fmt.Fprint(output, privacyNotice)
	confirmed, err := prompt.Confirm("Escribir la configuración confirmada", false)
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Fprintln(output, "Cambios cancelados. Los artefactos base creados en el paso 1 se conservan.")
		return nil
	}
	if err := backend.ApplySetup(ctx, snapshot, identity, access, secrets); err != nil {
		return fmt.Errorf("apply confirmed setup: %w", err)
	}

	fmt.Fprintln(output, "\n[9/9] Próximos pasos")
	fmt.Fprintln(output, "local-agent doctor")
	fmt.Fprintln(output, "local-agent doctor --live")
	fmt.Fprintln(output, "local-agent run")
	return nil
}

func displayList(values []string) string {
	if len(values) == 0 {
		return "(ninguno)"
	}
	return strings.Join(values, ", ")
}
