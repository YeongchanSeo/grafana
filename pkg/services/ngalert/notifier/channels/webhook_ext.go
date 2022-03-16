package channels

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/services/notifications"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/common/model"
)

// WebhookNotifier is responsible for sending
// alert notifications as webhooks.
type WebhookExtNotifier struct {
	*Base
	URL           string
	User          string
	Password      string
	HTTPMethod    string
	MaxAlerts     int
	UrlParameters map[string]interface{}
	log           log.Logger
	ns            notifications.WebhookSender
	tmpl          *template.Template
	orgID         int64
}

type WebhookExtConfig struct {
	*NotificationChannelConfig
	URL           string
	User          string
	Password      string
	HTTPMethod    string
	MaxAlerts     int
	UrlParameters map[string]interface{}
}

func WebHookExtFactory(fc FactoryConfig) (NotificationChannel, error) {
	cfg, err := NewWebHookExtConfig(fc.Config, fc.DecryptFunc)
	if err != nil {
		return nil, receiverInitError{
			Reason: err.Error(),
			Cfg:    *fc.Config,
		}
	}
	return NewWebHookExtNotifier(cfg, fc.NotificationService, fc.Template), nil
}

func NewWebHookExtConfig(config *NotificationChannelConfig, decryptFunc GetDecryptedValueFn) (*WebhookExtConfig, error) {
	url := config.Settings.Get("url").MustString()
	if url == "" {
		return nil, errors.New("could not find url property in settings")
	}
	return &WebhookExtConfig{
		NotificationChannelConfig: config,
		URL:                       url,
		User:                      config.Settings.Get("username").MustString(),
		Password:                  decryptFunc(context.Background(), config.SecureSettings, "password", config.Settings.Get("password").MustString()),
		HTTPMethod:                config.Settings.Get("httpMethod").MustString("POST"),
		MaxAlerts:                 config.Settings.Get("maxAlerts").MustInt(0),
		UrlParameters:             config.Settings.Get("urlParameters").MustMap(),
	}, nil
}

// NewWebHookExtNotifier is the constructor for
// the WebHookExt notifier.
func NewWebHookExtNotifier(config *WebhookExtConfig, ns notifications.WebhookSender, t *template.Template) *WebhookExtNotifier {
	return &WebhookExtNotifier{
		Base: NewBase(&models.AlertNotification{
			Uid:                   config.UID,
			Name:                  config.Name,
			Type:                  config.Type,
			DisableResolveMessage: config.DisableResolveMessage,
			Settings:              config.Settings,
		}),
		orgID:         config.OrgID,
		URL:           config.URL,
		User:          config.User,
		Password:      config.Password,
		HTTPMethod:    config.HTTPMethod,
		MaxAlerts:     config.MaxAlerts,
		UrlParameters: config.UrlParameters,
		log:           log.New("alerting.notifier.webhook"),
		ns:            ns,
		tmpl:          t,
	}
}

// Notify implements the Notifier interface.
func (wn *WebhookExtNotifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	groupKey, err := notify.ExtractGroupKey(ctx)
	if err != nil {
		return false, err
	}

	as, numTruncated := truncateAlerts(wn.MaxAlerts, as)
	var tmplErr error
	tmpl, data := TmplText(ctx, wn.tmpl, as, wn.log, &tmplErr)
	title := tmpl(DefaultMessageTitleEmbed)
	message := tmpl(`{{ template "default.message" . }}`)
	msg := &webhookMessage{
		Version:         "1",
		ExtendedData:    data,
		GroupKey:        groupKey.String(),
		TruncatedAlerts: numTruncated,
		OrgID:           wn.orgID,
		Title:           title,
		Message:         message,
	}
	if types.Alerts(as...).Status() == model.AlertFiring {
		msg.State = string(models.AlertStateAlerting)
	} else {
		msg.State = string(models.AlertStateOK)
	}

	if tmplErr != nil {
		wn.log.Warn("failed to template webhook message", "err", tmplErr.Error())
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return false, err
	}

	u, err := url.Parse(wn.URL)
	if err != nil {
		return false, err
	}
	query := u.Query()
	for key, value := range wn.UrlParameters {
		v := value.(string)
		v = strings.Replace(v, "${title}", title, -1)
		v = strings.Replace(v, "${message}", message, -1)
		query.Set(key, v)
	}
	u.RawQuery = query.Encode()

	cmd := &models.SendWebhookSync{
		Url:        u.String(),
		User:       wn.User,
		Password:   wn.Password,
		Body:       string(body),
		HttpMethod: wn.HTTPMethod,
	}

	if err := wn.ns.SendWebhookSync(ctx, cmd); err != nil {
		return false, err
	}

	return true, nil
}

func (wn *WebhookExtNotifier) SendResolved() bool {
	return !wn.GetDisableResolveMessage()
}
