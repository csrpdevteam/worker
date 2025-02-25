package tickets

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TicketsBot/common/permission"
	"github.com/TicketsBot/common/sentry"
	"github.com/TicketsBot/database"
	"github.com/TicketsBot/worker/bot/command"
	cmdcontext "github.com/TicketsBot/worker/bot/command/context"
	"github.com/TicketsBot/worker/bot/command/registry"
	"github.com/TicketsBot/worker/bot/constants"
	"github.com/TicketsBot/worker/bot/customisation"
	"github.com/TicketsBot/worker/bot/dbclient"
	"github.com/TicketsBot/worker/bot/logic"
	"github.com/TicketsBot/worker/bot/redis"
	"github.com/TicketsBot/worker/bot/utils"
	"github.com/TicketsBot/worker/i18n"
	"github.com/rxdn/gdl/objects/channel"
	"github.com/rxdn/gdl/objects/channel/embed"
	"github.com/rxdn/gdl/objects/interaction"
	"github.com/rxdn/gdl/rest"
)

type SwitchPanelCommand struct {
}

func (c SwitchPanelCommand) Properties() registry.Properties {
	return registry.Properties{
		Name:            "switchpanel",
		Description:     i18n.HelpSwitchPanel,
		Type:            interaction.ApplicationCommandTypeChatInput,
		PermissionLevel: permission.Support,
		Category:        command.Tickets,
		InteractionOnly: true,
		Arguments: command.Arguments(
			command.NewRequiredAutocompleteableArgument("panel", "Ticket panel to switch the ticket to", interaction.OptionTypeInteger, i18n.MessageInvalidUser, c.AutoCompleteHandler),
		),
		Timeout: constants.TimeoutOpenTicket,
	}
}

func (c SwitchPanelCommand) GetExecutor() interface{} {
	return c.Execute
}

func (SwitchPanelCommand) Execute(ctx *cmdcontext.SlashCommandContext, panelId int) {
	// Get ticket struct
	ticket, err := dbclient.Client.Tickets.GetByChannelAndGuild(ctx, ctx.ChannelId(), ctx.GuildId())
	if err != nil {
		ctx.HandleError(err)
		return
	}

	// Verify this is a ticket channel
	if ticket.UserId == 0 || ticket.ChannelId == nil {
		ctx.Reply(customisation.Red, i18n.Error, i18n.MessageNotATicketChannel)
		return
	}

	// Check rate limit
	ratelimitCtx, cancel := context.WithTimeout(ctx, time.Second*3)
	defer cancel()

	allowed, err := redis.TakeRenameRatelimit(ratelimitCtx, ctx.ChannelId())
	if err != nil {
		ctx.HandleError(err)
		return
	}

	if !allowed {
		ctx.Reply(customisation.Red, i18n.TitleRename, i18n.MessageRenameRatelimited)
		return
	}

	// Fetch new panel details
	panel, err := dbclient.Client.Panel.GetById(ctx, panelId)
	if err != nil {
		ctx.HandleError(err)
		return
	}

	// Ensure the panel belongs to the same guild
	if panel.PanelId == 0 || panel.GuildId != ctx.GuildId() {
		ctx.Reply(customisation.Red, i18n.Error, i18n.MessageSwitchPanelInvalidPanel)
		return
	}

	// Update panel ID in database
	if err := dbclient.Client.Tickets.SetPanelId(ctx, ctx.GuildId(), ticket.Id, panelId); err != nil {
		ctx.HandleError(err)
		return
	}

	// Get ticket claimer
	claimer, err := dbclient.Client.TicketClaims.Get(ctx, ticket.GuildId, ticket.Id)
	if err != nil {
		ctx.HandleError(err)
		return
	}

	// Generate new channel name
	channelName, err := logic.GenerateChannelName(ctx.Context, ctx, &panel, ticket.Id, ticket.UserId, utils.NilIfZero(claimer))
	if err != nil {
		ctx.HandleError(err)
		return
	}

	// Set new channel topic
	channelTopic := fmt.Sprintf("Panel: %s | Ticket ID: %d", panel.Title, ticket.Id)

	// Handle thread tickets separately
	if ticket.IsThread {
		data := rest.ModifyChannelData{
			Name:  channelName,
			Topic: &channelTopic,
		}

		if _, err := ctx.Worker().ModifyChannel(*ticket.ChannelId, data); err != nil {
			ctx.HandleError(err)
			return
		}

		ctx.ReplyRaw(customisation.Green, "Success", fmt.Sprintf("This ticket has been switched to the panel **%s**.\n\nNote: As this is a thread, the permissions could not be bulk updated.", panel.Title))
		return
	}

	// Fetch additional ticket members
	members, err := dbclient.Client.TicketMembers.Get(ctx, ctx.GuildId(), ticket.Id)
	if err != nil {
		ctx.HandleError(err)
		return
	}

	// Generate new permissions
	var overwrites []channel.PermissionOverwrite
	if claimer == 0 {
		overwrites, err = logic.CreateOverwrites(ctx.Context, ctx, ticket.UserId, &panel, members...)
	} else {
		overwrites, err = logic.GenerateClaimedOverwrites(ctx.Context, ctx.Worker(), ticket, claimer)
		if err != nil {
			ctx.HandleError(err)
			return
		}
		if overwrites == nil {
			overwrites, err = logic.CreateOverwrites(ctx.Context, ctx, ticket.UserId, &panel, members...)
		}
	}

	// Update channel with new settings
	data := rest.ModifyChannelData{
		Name:                 channelName,
		Topic:                &channelTopic, // <-- Updating the channel topic
		PermissionOverwrites: overwrites,
		ParentId:             panel.TargetCategory,
	}

	if _, err = ctx.Worker().ModifyChannel(*ticket.ChannelId, data); err != nil {
		ctx.HandleError(err)
		return
	}

	ctx.ReplyPermanent(customisation.Green, i18n.TitlePanelSwitched, i18n.MessageSwitchPanelSuccess, panel.Title, ctx.UserId())
}

// Auto-complete handler for selecting a panel
func (SwitchPanelCommand) AutoCompleteHandler(data interaction.ApplicationCommandAutoCompleteInteraction, value string) []interaction.ApplicationCommandOptionChoice {
	if data.GuildId.Value == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()

	panels, err := dbclient.Client.Panel.GetByGuild(ctx, data.GuildId.Value)
	if err != nil {
		sentry.Error(err)
		return nil
	}

	if value == "" {
		if len(panels) > 25 {
			return panelsToChoices(panels[:25])
		} else {
			return panelsToChoices(panels)
		}
	} else {
		var filtered []database.Panel
		for _, panel := range panels {
			if strings.Contains(strings.ToLower(panel.Title), strings.ToLower(value)) {
				filtered = append(filtered, panel)
			}

			if len(filtered) == 25 {
				break
			}
		}

		return panelsToChoices(filtered)
	}
}

// Converts panel list into auto-complete choices
func panelsToChoices(panels []database.Panel) []interaction.ApplicationCommandOptionChoice {
	choices := make([]interaction.ApplicationCommandOptionChoice, len(panels))
	for i, panel := range panels {
		choices[i] = interaction.ApplicationCommandOptionChoice{
			Name:  panel.Title,
			Value: panel.PanelId,
		}
	}

	return choices
}
