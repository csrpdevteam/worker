package admin

import (
  "fmt"
  "time"

  "github.com/TicketsBot/common/permission"
  "github.com/TicketsBot/worker/bot/command"
  "github.com/TicketsBot/worker/bot/command/registry"
  "github.com/TicketsBot/worker/bot/customisation"
  "github.com/TicketsBot/worker/i18n"
  "github.com/rxdn/gdl/objects/interaction"
  "github.com/rxdn/gdl/objects/channel"
)


var AllowedCategoryIDs = []uint64{
  1176986720772833421, // Gen Support
  1250633639545536523, // Internal Affair Inquiries
  1139516721166827531, // Rx queue
  1191433020868132924, // Partnership 
}

type AdminDeleteChannelCommand struct{}

func (AdminDeleteChannelCommand) Properties() registry.Properties {
  return registry.Properties{
    Name:            "forceclose",
    Description:     "Force closes the ticket",
    Type:            interaction.ApplicationCommandTypeChatInput,
    PermissionLevel: permission.Admin,
    Category:        command.Moderation,
    Timeout:         time.Second * 10,
  }
}

func (c AdminDeleteChannelCommand) GetExecutor() interface{} {
  return c.Execute
}

func (AdminDeleteChannelCommand) Execute(ctx registry.CommandContext) {
  channelId := ctx.ChannelId()

  ch, err := ctx.Worker().GetChannel(channelId)
  if err != nil {
    ctx.HandleError(err)
    return
  }


  if !isAllowedCategory(ch.ParentId.Value) {
    errorEmbed := channel.Embed{
      Title:       "Error",
      Description: "This is not a ticket channel.",
      Color:       customisation.Red,
    }

    _, err := ctx.ReplyWithEmbed(errorEmbed)
    if err != nil {
      ctx.HandleError(err)
    }
    return
  }

  embed := channel.Embed{
    Title:       "Admin",
    Description: "**Force closing ticket.**\n*The ticket is being force-closed due to a bug. Please wait, this may take up to 15 seconds.*\n\n -# Note: A transcript will not be saved for this ticket.",
    Color:       customisation.Green,
  }

  _, err = ctx.Worker().CreateMessageEmbed(channelId, embed)
  if err != nil {
    ctx.HandleError(err)
    return
  }

  time.Sleep(5 * time.Second)

  err = ctx.Worker().DeleteChannel(channelId)
  if err != nil {
    ctx.HandleError(err)
    return
  }
}


func isAllowedCategory(categoryID uint64) bool {
  for _, allowedID := range AllowedCategoryIDs {
    if categoryID == allowedID {
      return true
    }
  }
  return false
}
