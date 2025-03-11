package logic

import (
	"context"
	"errors"
	"fmt"
	"github.com/TicketsBot/common/collections"
	"github.com/TicketsBot/common/permission"
	"github.com/TicketsBot/common/sentry"
	"github.com/TicketsBot/database"
	"github.com/TicketsBot/worker/bot/command/registry"
	"github.com/TicketsBot/worker/bot/customisation"
	"github.com/TicketsBot/worker/bot/dbclient"
	"github.com/TicketsBot/worker/bot/metrics/statsd"
	"github.com/TicketsBot/worker/bot/redis"
	"github.com/TicketsBot/worker/bot/utils"
	"github.com/TicketsBot/worker/i18n"
	"github.com/rxdn/gdl/objects/channel/embed"
	"github.com/rxdn/gdl/objects/channel/message"
	"github.com/rxdn/gdl/objects/member"
	"github.com/rxdn/gdl/rest"
	"github.com/rxdn/gdl/rest/request"
	"net/http"
	"time"
)

func CloseTicket(ctx context.Context, cmd registry.CommandContext, reason *string, bypassPermissionCheck bool) error {
	var success bool
	errorContext := cmd.ToErrorContext()

	// Get ticket struct
	ticket, err := dbclient.Client.Tickets.GetByChannelAndGuild(ctx, cmd.ChannelId(), cmd.GuildId())
	if err != nil {
		return err
	}

	// Check if it's a valid ticket or in an allowed category
	if ticket.Id == 0 || ticket.GuildId != cmd.GuildId() {
		// Fetch the channel details
		channel, err := cmd.Worker().GetChannel(cmd.ChannelId())
		if err != nil {
			return err
		}

		allowedCategories := map[uint64]bool{
			1176986720772833421: true, // General Inquiry Tickets
			1250633639545536523: true, // Internal Affair Inquiries
			1139516721166827531: true, // Rx queue
			1191433020868132924: true, // Partnership Inquiry
		}

		// Check if the channel is in an allowed category
		if channel.ParentId.IsNull || !allowedCategories[channel.ParentId.Value] {
			cmd.Reply(customisation.Red, i18n.Error, i18n.MessageNotATicketChannel)
			return nil
		}
	}

	defer func() {
		if !success {
			if err := dbclient.Client.AutoCloseExclude.Exclude(ctx, ticket.GuildId, ticket.Id); err != nil {
				sentry.ErrorWithContext(err, errorContext)
			}
		}
	}()

	if !bypassPermissionCheck && !utils.CanClose(ctx, cmd, ticket) {
		cmd.Reply(customisation.Red, i18n.Error, i18n.MessageCloseNoPermission)
		return nil
	}

	member, err := cmd.Member()
	if err != nil {
		return err
	}

	settings, err := cmd.Settings()
	if err != nil {
		return err
	}

	// Check the channel still exists - if it does not, just set to closed in the database, as this must be a request
	// from the dashboard for a ticket with a channel that does not exist.
	if cmd.Source() == registry.SourceDashboard {
		channelExists, err := checkChannelExists(cmd, ticket)
		if err != nil {
			return err
		}

		if !channelExists {
			if err := dbclient.Client.Tickets.Close(ctx, ticket.Id, ticket.GuildId); err != nil {
				return err
			}

			return nil
		}
	}

	// Archive
	if settings.StoreTranscripts {
		msgs := make([]message.Message, 0, 50)

		const limit = 100

		lastId := uint64(0)
		lastChunkSize := limit
		for lastChunkSize == limit {
			chunk, err := cmd.Worker().GetChannelMessages(cmd.ChannelId(), rest.GetChannelMessagesData{
				Before: lastId,
				Limit:  limit,
			})

			if err != nil {
				// First rest interaction, check for 403
				var restError request.RestError
				if errors.As(err, &restError) && restError.StatusCode == 403 {
					if err := dbclient.Client.AutoCloseExclude.ExcludeAll(ctx, cmd.GuildId()); err != nil {
						sentry.ErrorWithContext(err, errorContext)
					}
				}

				return err
			}

			lastChunkSize = len(chunk)

			if lastChunkSize > 0 {
				lastId = chunk[len(chunk)-1].Id
				msgs = append(msgs, chunk...)
			}
		}

		// Reverse messages
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}

		// Update participants, incase the websocket gateway missed any messages
		participants := collections.NewSet[uint64]()
		for _, msg := range msgs {
			participants.Add(msg.Author.Id)
		}

		if err := dbclient.Client.Participants.SetBulk(ctx, cmd.GuildId(), ticket.Id, participants.Collect()); err != nil {
			return err
		}

		if err := utils.ArchiverClient.Store(ctx, cmd.GuildId(), ticket.Id, msgs); err != nil {
			return err
		}

		if err := dbclient.Client.Tickets.SetHasTranscript(ctx, cmd.GuildId(), ticket.Id, true); err != nil {
			return err
		}
	}

	// Set ticket state as closed and delete channel
	if err := dbclient.Client.Tickets.Close(ctx, ticket.Id, cmd.GuildId()); err != nil {
		return err
	}

	success = true
	ticket.CloseTime = utils.Ptr(time.Now())

	// set close reason + user
	closeMetadata := database.CloseMetadata{
		Reason: reason,
	}

	if cmd.UserId() != cmd.Worker().BotId {
		closeMetadata.ClosedBy = utils.Ptr(cmd.UserId())
	}

	if err := dbclient.Client.CloseReason.Set(ctx, cmd.GuildId(), ticket.Id, closeMetadata); err != nil {
		return err
	}

	if ticket.IsThread {
		// If it is a thread, we need to send a message
		if reason == nil {
			cmd.ReplyPermanent(customisation.Green, i18n.TitleTicketClosed, i18n.MessageCloseSuccess, cmd.UserId())
		} else {
			fields := []embed.EmbedField{
				{
					Name:   cmd.GetMessage(i18n.Reason),
					Value:  fmt.Sprintf("```%s```", *reason),
					Inline: false,
				},
			}

			cmd.ReplyWithFieldsPermanent(customisation.Green, i18n.TitleTicketClosed, i18n.MessageCloseSuccess, fields, cmd.UserId())
		}

		// Discord has a race condition
		time.Sleep(time.Millisecond * 250)

		data := rest.ModifyChannelData{
			ThreadMetadataModifyData: &rest.ThreadMetadataModifyData{
				Archived: utils.Ptr(true),
				Locked:   utils.Ptr(true),
			},
		}

		if _, err := cmd.Worker().ModifyChannel(cmd.ChannelId(), data); err != nil {
			return err
		}
	} else {
		if _, err := cmd.Worker().DeleteChannel(cmd.ChannelId()); err != nil {
			// Check if we should exclude this from autoclose
			var restError request.RestError
			if errors.As(err, &restError) && restError.StatusCode == 403 {
				if err := dbclient.Client.AutoCloseExclude.Exclude(ctx, ticket.GuildId, ticket.Id); err != nil {
					sentry.ErrorWithContext(err, errorContext)
				}
			}

			return err
		}
	}

	// Save space - delete the webhook
	if !ticket.IsThread {
		go dbclient.Client.Webhooks.Delete(ctx, cmd.GuildId(), ticket.Id)
	}

	if err := dbclient.Client.CloseRequest.Delete(ctx, ticket.GuildId, ticket.Id); err != nil {
		sentry.ErrorWithContext(err, cmd.ToErrorContext())
	}

	// Delete join thread button
	if ticket.IsThread && ticket.JoinMessageId != nil && settings.TicketNotificationChannel != nil {
		_ = cmd.Worker().DeleteMessage(*settings.TicketNotificationChannel, *ticket.JoinMessageId)
		if err := dbclient.Client.Tickets.SetJoinMessageId(ctx, ticket.GuildId, ticket.Id, nil); err != nil {
			sentry.ErrorWithContext(err, errorContext)
		}
	}

	if err := sendCloseEmbed(ctx, cmd, errorContext, member, settings, ticket, reason); err != nil {
		return err
	}

	return nil
}

func sendCloseEmbed(ctx context.Context, cmd registry.CommandContext, errorContext sentry.ErrorContext, member member.Member, settings database.Settings, ticket database.Ticket, reason *string) error {
	// Send logs to archive channel
	archiveChannelId, err := dbclient.Client.ArchiveChannel.Get(ctx, ticket.GuildId)
	if err != nil {
		sentry.ErrorWithContext(err, errorContext)
		return err
	}

	var archiveChannelExists bool
	if archiveChannelId != nil {
		if _, err := cmd.Worker().GetChannel(*archiveChannelId); err == nil {
			archiveChannelExists = true
		}
	}

	if archiveChannelExists && archiveChannelId != nil {
		componentBuilders := [][]CloseEmbedElement{
			{
				TranscriptLinkElement(settings.StoreTranscripts),
				ThreadLinkElement(ticket.IsThread && ticket.ChannelId != nil),
			},
		}

		closeEmbed, closeComponents := BuildCloseEmbed(ctx, cmd.Worker(), ticket, member.User.Id, reason, nil, componentBuilders)

		data := rest.CreateMessageData{
			Embeds:     utils.Slice(closeEmbed),
			Components: closeComponents,
		}

		msg, err := cmd.Worker().CreateMessageComplex(*archiveChannelId, data)
		if err != nil {
			sentry.ErrorWithContext(err, errorContext)
			return err
		} else {
			// Add message to archive
			if err := dbclient.Client.ArchiveMessages.Set(ctx, ticket.GuildId, ticket.Id, *archiveChannelId, msg.Id); err != nil {
				return err
			}
		}
	}

	// Notify user and send logs in DMs
	// This mutates state!
	dmChannel, ok := getDmChannel(cmd, ticket.UserId)
	if ok {
		guild, err := cmd.Guild()
		if err != nil {
			sentry.ErrorWithContext(err, errorContext)
			return err
		}

		feedbackEnabled, err := dbclient.Client.FeedbackEnabled.Get(ctx, cmd.GuildId())
		if err != nil {
			sentry.ErrorWithContext(err, errorContext)
			return err
		}

		// Only offer to take feedback if the user has sent a message
		hasSentMessage, err := dbclient.Client.Participants.HasParticipated(ctx, cmd.GuildId(), ticket.Id, ticket.UserId)
		if err != nil {
			sentry.ErrorWithContext(err, errorContext)
			return err
		}

		openerMember, err := cmd.Worker().GetGuildMember(cmd.GuildId(), ticket.UserId)
		if err != nil {
			var restError request.RestError
			if errors.As(err, &restError) {
				if restError.StatusCode != 404 { // User left the server
					sentry.ErrorWithContext(err, errorContext)
					return err
				}
			} else {
				sentry.ErrorWithContext(err, errorContext)
				return err
			}
		}

		// Only offer to take feedback if the user is *not* staff
		permLevel, err := permission.GetPermissionLevel(ctx, utils.ToRetriever(cmd.Worker()), openerMember, cmd.GuildId())
		if err != nil {
			sentry.ErrorWithContext(err, errorContext)
			return err
		}

		statsd.Client.IncrementKey(statsd.KeyDirectMessage)

		componentBuilders := [][]CloseEmbedElement{
			{
				TranscriptLinkElement(settings.StoreTranscripts),
				ThreadLinkElement(ticket.IsThread && ticket.ChannelId != nil),
			},
			{
				FeedbackRowElement(feedbackEnabled && hasSentMessage && permLevel == permission.Everyone),
			},
		}

		closeEmbed, closeComponents := BuildCloseEmbed(ctx, cmd.Worker(), ticket, member.User.Id, reason, nil, componentBuilders)
		closeEmbed.SetAuthor(guild.Name, "", fmt.Sprintf("https://cdn.discordapp.com/icons/%d/%s.png", guild.Id, guild.Icon))

		// Use message content to tell users why they can't rate a ticket
		var content string
		if feedbackEnabled {
			if permLevel > permission.Everyone {
				content = "-# " + cmd.GetMessage(i18n.MessageCloseCantRateStaff, guild.Name)
			} else if !hasSentMessage {
				content = "-# " + cmd.GetMessage(i18n.MessageCloseCantRateEmpty)
			}
		}

		data := rest.CreateMessageData{
			Content:    content,
			Embeds:     utils.Slice(closeEmbed),
			Components: closeComponents,
		}

		if _, err := cmd.Worker().CreateMessageComplex(dmChannel, data); err != nil {
			sentry.ErrorWithContext(err, errorContext)
			return err
		}
	}

	return nil
}

func getDmChannel(ctx registry.CommandContext, userId uint64) (uint64, bool) {
	// Hack for autoclose
	if ctx.Worker().BotId == userId {
		return 0, false
	}

	cachedId, err := redis.GetDMChannel(userId, ctx.Worker().BotId)
	if err != nil {
		if err != redis.ErrNotCached {
			sentry.ErrorWithContext(err, ctx.ToErrorContext())
		}
		return 0, false
	}

	if cachedId == nil {
		return 0, false
	}

	return *cachedId, true
}

func checkChannelExists(ctx registry.CommandContext, ticket database.Ticket) (bool, error) {
	if ticket.ChannelId == nil {
		return false, nil
	}

	// If the channel does not exist, it will trigger a cache miss and then attempt to fetch it from the API
	if _, err := ctx.Worker().GetChannel(*ticket.ChannelId); err != nil {
		var restError request.RestError
		if errors.As(err, &restError) && restError.StatusCode == http.StatusNotFound {
			return false, nil
		}

		return false, err
	}

	return true, nil
}
