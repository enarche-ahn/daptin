package server

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/artpar/api2go"
	"github.com/daptin/daptin/server/auth"
	"github.com/daptin/daptin/server/resource"
	"github.com/flashmob/go-guerrilla/backends"
	"github.com/flashmob/go-guerrilla/mail"
	"github.com/flashmob/go-guerrilla/response"
	"math/big"
	"net"
	"net/http"
	"strings"
)

// ----------------------------------------------------------------------------------
// Processor Name: sql
// ----------------------------------------------------------------------------------
// Description   : Saves the e.Data (email data) and e.DeliveryHeader together in sql
//               : using the hash generated by the "hash" processor and stored in
//               : e.Hashes
// ----------------------------------------------------------------------------------
// Config Options: mail_table string - name of table for storing emails
//               : sql_driver string - database driver name, eg. mysql
//               : sql_dsn string - driver-specific data source name
//               : primary_mail_host string - primary host name
// --------------:-------------------------------------------------------------------
// Input         : e.Data
//               : e.DeliveryHeader generated by ParseHeader() processor
//               : e.MailFrom
//               : e.Subject - generated by by ParseHeader() processor
// ----------------------------------------------------------------------------------
// Output        : Sets e.QueuedId with the first item fromHashes[0]
// ----------------------------------------------------------------------------------

type stmtCache [backends.GuerrillaDBAndRedisBatchMax]*sql.Stmt

type SQLProcessorConfig struct {
	PrimaryHost string `json:"primary_mail_host"`
	DbResource  *resource.DbResource
}

type SQLProcessor struct {
	cache  stmtCache
	config *SQLProcessorConfig
}

// for storing ip addresses in the ip_addr column
func (s *SQLProcessor) ip2bint(ip string) *big.Int {
	bint := big.NewInt(0)
	addr := net.ParseIP(ip)
	if strings.Index(ip, "::") > 0 {
		bint.SetBytes(addr.To16())
	} else {
		bint.SetBytes(addr.To4())
	}
	return bint
}

func (s *SQLProcessor) fillAddressFromHeader(e *mail.Envelope, headerKey string) string {
	if v, ok := e.Header[headerKey]; ok {
		addr, err := mail.NewAddress(v[0])
		if err != nil {
			return ""
		}
		return addr.String()
	}
	return ""
}

// compressedData struct will be compressed using zlib when printed via fmt
type Compressor interface {
	String() string
}

func trimToLimit(str string, limit int) string {
	ret := strings.TrimSpace(str)
	if len(str) > limit {
		ret = str[:limit]
	}
	return ret
}

func DaptinSQLDbResource(dbResource *resource.DbResource) func() backends.Decorator {

	return func() backends.Decorator {
		var config *SQLProcessorConfig
		//var db *sql.DB
		s := &SQLProcessor{}

		// open the database connection (it will also check if we can select the table)
		backends.Svc.AddInitializer(backends.InitializeWith(func(backendConfig backends.BackendConfig) error {
			configType := backends.BaseConfig(&SQLProcessorConfig{})
			bcfg, err := backends.Svc.ExtractConfig(backendConfig, configType)
			if err != nil {
				return err
			}
			config = bcfg.(*SQLProcessorConfig)
			s.config = config
			return nil
		}))

		// shutdown will close the database connection
		backends.Svc.AddShutdowner(backends.ShutdownWith(func() error {
			//if db != nil {
			//	return db.Close()
			//}
			return nil
		}))

		return func(p backends.Processor) backends.Processor {
			return backends.ProcessWith(func(e *mail.Envelope, task backends.SelectTask) (backends.Result, error) {

				if task == backends.TaskSaveMail {
					var to, body string

					hash := ""
					if len(e.Hashes) > 0 {
						hash = e.Hashes[0]
						e.QueuedId = e.Hashes[0]
					}

					var co Compressor
					// a compressor was set by the Compress processor
					if c, ok := e.Values["zlib-compressor"]; ok {
						body = "gzip"
						co = c.(Compressor)
					}
					// was saved in redis by the Redis processor
					if _, ok := e.Values["redis"]; ok {
						body = "redis"
					}

					for i := range e.RcptTo {
						// use the To header, otherwise rcpt to
						to = trimToLimit(s.fillAddressFromHeader(e, "To"), 255)
						if to == "" {
							// trimToLimit(strings.TrimSpace(e.RcptTo[i].User)+"@"+config.PrimaryHost, 255)
							to = trimToLimit(strings.TrimSpace(e.RcptTo[i].String()), 255)
						}
						mid := trimToLimit(s.fillAddressFromHeader(e, "Message-Id"), 255)
						if mid == "" {
							mid = fmt.Sprintf("%s.%s@%s", hash, e.RcptTo[i].User, config.PrimaryHost)
						}
						// replyTo is the 'Reply-to' header, it may be blank
						replyTo := trimToLimit(s.fillAddressFromHeader(e, "Reply-To"), 255)
						// sender is the 'Sender' header, it may be blank
						sender := trimToLimit(s.fillAddressFromHeader(e, "Sender"), 255)

						recipient := trimToLimit(strings.TrimSpace(e.RcptTo[i].String()), 255)
						contentType := ""
						if v, ok := e.Header["Content-Type"]; ok {
							contentType = trimToLimit(v[0], 255)
						}

						var mailBody interface{}
						// `mail` column
						if body == "redis" {
							// data already saved in redis
							mailBody = ""
						} else if co != nil {
							// use a compressor (automatically adds e.DeliveryHeader)
							mailBody = co.String()
						} else {
							mailBody = e.String()
						}
						pr := &http.Request{}

						user, err := dbResource.GetUserAccountRowByEmail(to)

						sessionUser := &auth.SessionUser{
						}

						if err == nil {

							sessionUser = &auth.SessionUser{
								UserId:          user["id"].(int64),
								UserReferenceId: user["reference_id"].(string),
								Groups:          []auth.GroupPermission{},
							}
						}

						pr = pr.WithContext(context.WithValue(context.Background(), "user", sessionUser))

						req := &api2go.Request{
							PlainRequest: pr,
						}

						model := api2go.Api2GoModel{
							Data: map[string]interface{}{
								"message_id":       mid,
								"mail_id":          hash,
								"from_address":     trimToLimit(e.MailFrom.String(), 255),
								"to_address":       to,
								"sender_address":   sender,
								"subject":          trimToLimit(e.Subject, 255),
								"body":             body,
								"mail":             mailBody,
								"spam_score":       0,
								"hash":             hash,
								"content_type":     contentType,
								"reply_to_address": replyTo,
								"recipient":        recipient,
								"has_attachment":   0,
								"ip_addr":          s.ip2bint(e.RemoteIP).Bytes(),
								"return_path":      trimToLimit(e.MailFrom.String(), 255),
								"is_tls":           e.TLS,
							},
						}
						_, err = dbResource.CreateWithoutFilter(&model, *req)

						if err != nil {
							return backends.NewResult(fmt.Sprint("554 Error: could not save email")), backends.StorageError
						}
					}

					// continue to the next Processor in the decorator chain
					return p.Process(e, task)
				} else if task == backends.TaskValidateRcpt {
					// if you need to validate the e.Rcpt then change to:
					if len(e.RcptTo) > 0 {
						// since this is called each time a recipient is added
						// validate only the _last_ recipient that was appended
						last := e.RcptTo[len(e.RcptTo)-1]
						if len(last.User) > 255 {
							// return with an error
							return backends.NewResult(response.Canned.FailRcptCmd), backends.NoSuchUser
						}
					}
					// continue to the next processor
					return p.Process(e, task)
				} else {
					return p.Process(e, task)
				}
			})
		}
	}

}
