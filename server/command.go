package main

import (
	"fmt"
	"strings"

	"github.com/mattermost/mattermost-plugin-zoom/server/zoom"

	"github.com/mattermost/mattermost-plugin-api/experimental/command"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/plugin"
	"github.com/pkg/errors"
)

const (
	starterText        = "###### Mattermost Zoom Plugin - Slash Command Help\n"
	helpText           = `* |/zoom start| - Start a zoom meeting`
	oAuthHelpText      = `* |/zoom disconnect| - Disconnect from zoom`
	settingHelpText    = `* |/zoom setting| - Configure setting options`
	settingPMIHelpText = `* |/zoom setting use_pmi [true/false/ask]| - 
		enable / disable / undecide to use PMI to create meeting
	`
	alreadyConnectedText   = "Already connected"
	zoomPreferenceCategory = "plugin:zoom"
	zoomPMISettingName     = "use-pmi"
	zoomPMISettingValueAsk = "ask"
	preferenceUpdateError  = "Cannot update preference in zoom setting"
)

const (
	actionConnect    = "connect"
	actionStart      = "start"
	actionDisconnect = "disconnect"
	actionHelp       = "help"
)

func (p *Plugin) getCommand() (*model.Command, error) {
	iconData, err := command.GetIconData(p.API, "assets/profile.svg")
	if err != nil {
		return nil, errors.Wrap(err, "failed to get icon data")
	}

	return &model.Command{
		Trigger:              "zoom",
		AutoComplete:         true,
		AutoCompleteDesc:     "Available commands: start, disconnect, help, setting",
		AutoCompleteHint:     "[command]",
		AutocompleteData:     p.getAutocompleteData(),
		AutocompleteIconData: iconData,
	}, nil
}

func (p *Plugin) postCommandResponse(args *model.CommandArgs, text string) {
	post := &model.Post{
		UserId:    p.botUserID,
		ChannelId: args.ChannelId,
		Message:   text,
	}
	_ = p.API.SendEphemeralPost(args.UserId, post)
}

func (p *Plugin) parseCommand(rawCommand string) (cmd, action, topic string) {
	split := strings.Fields(rawCommand)
	cmd = split[0]
	if len(split) > 1 {
		action = split[1]
	}
	if action == actionStart {
		topic = strings.Join(split[2:], " ")
	}
	return cmd, action, topic
}

func (p *Plugin) executeCommand(c *plugin.Context, args *model.CommandArgs) (string, error) {
	split := strings.Fields(args.Command)
	command, action, topic := p.parseCommand(args.Command)

	if command != "/zoom" {
		return fmt.Sprintf("Command '%s' is not /zoom. Please try again.", command), nil
	}

	if action == "" {
		return "Please specify an action for /zoom command.", nil
	}

	userID := args.UserId
	user, appErr := p.API.GetUser(userID)
	if appErr != nil {
		return fmt.Sprintf("We could not retrieve user (userId: %v)", args.UserId), nil
	}

	switch action {
	case actionConnect:
		return p.runConnectCommand(user, args)
	case actionStart:
		return p.runStartCommand(args, user, topic)
	case actionDisconnect:
		return p.runDisconnectCommand(user)
	case actionHelp, "":
		return p.runHelpCommand()
	case "setting":
		return p.runSettingCommand(split[2:], user)
	default:
		return fmt.Sprintf("Unknown action %v", action), nil
	}
}

func (p *Plugin) canConnect(user *model.User) bool {
	return p.OAuthEnabled() && // we are not on JWT
		(!p.configuration.AccountLevelApp || // we are on user managed app
			user.IsSystemAdmin()) // admins can connect Account level apps
}

func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	msg, err := p.executeCommand(c, args)
	if err != nil {
		p.API.LogWarn("failed to execute command", "error", err.Error())
	}
	if msg != "" {
		p.postCommandResponse(args, msg)
	}
	return &model.CommandResponse{}, nil
}

// runStartCommand runs command to start a Zoom meeting.
func (p *Plugin) runStartCommand(args *model.CommandArgs, user *model.User, topic string) (string, error) {
	if _, appErr := p.API.GetChannelMember(args.ChannelId, user.Id); appErr != nil {
		return fmt.Sprintf("We could not get channel members (channelId: %v)", args.ChannelId), nil
	}

	recentMeeting, recentMeetingLink, creatorName, provider, appErr := p.checkPreviousMessages(args.ChannelId)
	if appErr != nil {
		return "Error checking previous messages", nil
	}

	if recentMeeting {
		p.postConfirm(recentMeetingLink, args.ChannelId, topic, user.Id, args.RootId, creatorName, provider)
		return "", nil
	}

	zoomUser, authErr := p.authenticateAndFetchZoomUser(user)
	if authErr != nil {
		// the user state will be needed later while connecting the user to Zoom via OAuth
		if appErr := p.storeOAuthUserState(user.Id, args.ChannelId, false); appErr != nil {
			p.API.LogWarn("failed to store user state")
		}
		return authErr.Message, authErr.Err
	}
	var meetingID int = -1
	var createMeetingErr error = nil

	if userPMISettingPref, getUserPMISettingErr := p.getPMISettingData(user.Id); getUserPMISettingErr == nil {
		switch userPMISettingPref {
		case "", zoomPMISettingValueAsk:
			p.askUserPMIMeeting(user.Id, args.ChannelId)
		case trueString:
			meetingID = zoomUser.Pmi
		default:
			meetingID, createMeetingErr = p.createMeetingWithoutPMI(
				user, zoomUser, args.ChannelId, defaultMeetingTopic,
			)
		}
	} else {
		p.askUserPMIMeeting(user.Id, args.ChannelId)
	}
	if meetingID == -1 && createMeetingErr == nil {
		return "", nil
	}
	if meetingID == -1 || createMeetingErr != nil {
		return "", errors.New("error while create new meeting")
	}
	if postMeetingErr := p.postMeeting(user, meetingID, args.ChannelId, args.RootId, defaultMeetingTopic); postMeetingErr != nil {
		return "", postMeetingErr
	}

	p.trackMeetingStart(args.UserId, telemetryStartSourceCommand)

	return "", nil
}

func (p *Plugin) runConnectCommand(user *model.User, extra *model.CommandArgs) (string, error) {
	if !p.canConnect(user) {
		return "Unknown action `connect`", nil
	}

	oauthMsg := fmt.Sprintf(
		zoom.OAuthPrompt,
		*p.API.GetConfig().ServiceSettings.SiteURL)

	// OAuth Account Level
	if p.configuration.AccountLevelApp {
		token, err := p.getSuperuserToken()
		if err == nil && token != nil {
			return alreadyConnectedText, nil
		}

		appErr := p.storeOAuthUserState(user.Id, extra.ChannelId, true)
		if appErr != nil {
			return "", errors.Wrap(appErr, "cannot store state")
		}
		return oauthMsg, nil
	}

	// OAuth User Level
	_, err := p.fetchOAuthUserInfo(zoomUserByMMID, user.Id)
	if err == nil {
		return alreadyConnectedText, nil
	}

	appErr := p.storeOAuthUserState(user.Id, extra.ChannelId, true)
	if appErr != nil {
		return "", errors.Wrap(appErr, "cannot store state")
	}
	return oauthMsg, nil
}

// runDisconnectCommand runs command to disconnect from Zoom. Will fail if user cannot connect.
func (p *Plugin) runDisconnectCommand(user *model.User) (string, error) {
	if !p.canConnect(user) {
		return "Unknown action `disconnect`", nil
	}

	if p.configuration.AccountLevelApp {
		err := p.removeSuperUserToken()
		if err != nil {
			return "Error disconnecting, " + err.Error(), nil
		}
		return "Successfully disconnected from Zoom.", nil
	}

	err := p.disconnectOAuthUser(user.Id)

	if err != nil {
		return "Could not disconnect OAuth from zoom, " + err.Error(), nil
	}

	p.trackDisconnect(user.Id)

	return "User disconnected from Zoom.", nil
}

// runHelpCommand runs command to display help text.
func (p *Plugin) runHelpCommand() (string, error) {
	text := starterText
	text += strings.ReplaceAll(helpText+settingHelpText, "|", "`")
	if p.configuration.EnableOAuth {
		text += "\n" + strings.ReplaceAll(oAuthHelpText, "|", "`")
	}

	return text, nil
}

// run "/zoom setting" command, e.g: /zoom setting use_pmi true
func (p *Plugin) runSettingCommand(settingArgs []string, user *model.User) (string, error) {
	settingAction := ""
	if len(settingArgs) > 0 {
		settingAction = settingArgs[0]
	}
	switch settingAction {
	case "use_pmi":
		// here process the use_pmi command
		if len(settingArgs) > 1 {
			return p.runPMISettingCommand(settingArgs[1], user)
		}
		return "Set PMI option to \"true\"|\"false\"|\"ask\"", nil
	case "":
		return strings.ReplaceAll(starterText+settingPMIHelpText, "|", "`"), nil
	default:
		return fmt.Sprintf("Unknown Action %v", settingAction), nil
	}
}

func (p *Plugin) runPMISettingCommand(usePMIValue string, user *model.User) (string, error) {
	switch usePMIValue {
	case trueString, falseString, zoomPMISettingValueAsk:
		if appError := p.API.UpdatePreferencesForUser(user.Id, []model.Preference{
			{
				UserId:   user.Id,
				Category: zoomPreferenceCategory,
				Name:     zoomPMISettingName,
				Value:    usePMIValue,
			},
		}); appError != nil {
			return preferenceUpdateError, nil
		}
		return fmt.Sprintf("Update successfully, use_pmi: %v", usePMIValue), nil
	default:
		return fmt.Sprintf("Unknown setting option %v", usePMIValue), nil
	}
}

// getAutocompleteData retrieves auto-complete data for the "/zoom" command
func (p *Plugin) getAutocompleteData() *model.AutocompleteData {
	available := "start, help, setting"
	if p.configuration.EnableOAuth && !p.configuration.AccountLevelApp {
		available = "start, connect, disconnect, help, setting"
	}
	zoom := model.NewAutocompleteData("zoom", "[command]", fmt.Sprintf("Available commands: %s", available))

	start := model.NewAutocompleteData("start", "[meeting topic]", "Starts a Zoom meeting")
	zoom.AddCommand(start)

	// no point in showing the 'disconnect' option if OAuth is not enabled
	if p.OAuthEnabled() && !p.configuration.AccountLevelApp {
		connect := model.NewAutocompleteData("connect", "", "Connect to Zoom")
		disconnect := model.NewAutocompleteData("disconnect", "", "Disonnects from Zoom")
		zoom.AddCommand(connect)
		zoom.AddCommand(disconnect)
	}

	// setting to allow the user decide whether to use its PMI on instant meetings.
	setting := model.NewAutocompleteData("setting", "[command]", "Configurates options")
	zoom.AddCommand(setting)

	// usePMI seting so user can choose to use their PMI or new ID to create new meeting in zoom.
	usePMI := model.NewAutocompleteData("use_pmi", "", "Use Personal Meeting ID")
	usePMIItems := []model.AutocompleteListItem{{
		HelpText: "Ask to start meeting with or without using Personal Meeting ID",
		Item:     "ask",
	}, {
		HelpText: "Start meeting using Personal Meeting ID",
		Item:     "true",
	}, {
		HelpText: "Start meeting without using Personal Meeting ID",
		Item:     "false",
	}}
	usePMI.AddStaticListArgument("", false, usePMIItems)
	setting.AddCommand(usePMI)

	// help to help the user if got stuck in understanding the commands
	help := model.NewAutocompleteData("help", "", "Display usage")
	zoom.AddCommand(help)

	return zoom
}
