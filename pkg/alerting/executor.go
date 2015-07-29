package alerting

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/grafana/grafana/pkg/api"
	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/graphite"
	"github.com/grafana/grafana/pkg/log"
	m "github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/services/rabbitmq"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/hashicorp/golang-lru"
	"github.com/streadway/amqp"

	bgraphite "bosun.org/graphite"
)

type GraphiteReturner func(org_id int64) (bgraphite.Context, error)

func GraphiteAuthContextReturner(org_id int64) (bgraphite.Context, error) {
	u, err := url.Parse(setting.GraphiteUrl)
	if err != nil {
		return nil, fmt.Errorf("could not parse graphiteUrl: %q", err)
	}
	u.Path = path.Join(u.Path, "render/")
	ctx := graphite.GraphiteContext{
		Host: u.String(),
		Header: http.Header{
			"X-Org-Id": []string{fmt.Sprintf("%d", org_id)},
		},
	}
	return &ctx, nil
}

func ChanExecutor(fn GraphiteReturner, jobQueue JobQueue, cache *lru.Cache) {
	executorNum.Inc(1)
	defer executorNum.Dec(1)

	realQueue := jobQueue.(internalJobQueue).queue

	for job := range realQueue {
		jobQueueInternalItems.Value(int64(len(realQueue)))
		jobQueueInternalSize.Value(int64(setting.InternalJobQueueSize))
		execute(fn, job, cache)
	}
}

// AmqpExecutor reads jobs from rabbitmq, executes them, and acknowledges them
// if they processed succesfully or encountered a fatal error
// (i.e. an error that we know won't recover on future retries, so no point in retrying)
func AmqpExecutor(fn GraphiteReturner, consumer rabbitmq.Consumer, cache *lru.Cache) {
	executorNum.Inc(1)
	defer executorNum.Dec(1)
	consumer.Consume(func(msg *amqp.Delivery) error {
		job := Job{}
		if err := json.Unmarshal(msg.Body, &job); err != nil {
			log.Error(0, "failed to unmarshal msg body.", err)
			return nil
		}
		job.StoreMetricFunc = api.StoreMetric
		err := execute(GraphiteAuthContextReturner, &job, cache)
		if err != nil {
			if strings.HasPrefix(err.Error(), "fatal:") {
				log.Error(0, err.Error()+". removing job from queue")
				return nil
			}
			log.Error(0, err.Error()+". not acking message. retry later")
		}
		return err
	})
}

// execute executes an alerting job and returns any errors.
// errors are always prefixed with 'non-fatal' (i.e. error condition that imply retrying the job later might fix it)
// or 'fatal', when we're sure the job will never process successfully.
func execute(fn GraphiteReturner, job *Job, cache *lru.Cache) error {
	key := fmt.Sprintf("%d-%d", job.MonitorId, job.LastPointTs.Unix())

	preConsider := time.Now()

	if found, _ := cache.ContainsOrAdd(key, true); found {
		log.Debug("T %s already done", key)
		executorNumAlreadyDone.Inc(1)
		executorConsiderJobAlreadyDone.Value(time.Since(preConsider))
		return nil
	}

	log.Debug("T %s doing", key)
	executorNumOriginalTodo.Inc(1)
	executorConsiderJobOriginalTodo.Value(time.Since(preConsider))
	gr, err := fn(job.OrgId)
	if err != nil {
		return fmt.Errorf("fatal: job %q: %q", job, err)
	}

	preExec := time.Now()
	executorJobExecDelay.Value(preExec.Sub(job.LastPointTs))
	evaluator, err := NewGraphiteCheckEvaluator(gr, job.Definition)
	if err != nil {
		// expressions should be validated before they are stored in the db!
		return fmt.Errorf("fatal: job %q: invalid check definition %q: %q", job, job.Definition, err)
	}

	res, err := evaluator.Eval(job.LastPointTs)
	log.Debug("job results - job:%v err:%v res:%v", job, err, res)
	if err != nil {
		return fmt.Errorf("%s , Eval failed for job %q", err.Error(), job)
	}

	durationExec := time.Since(preExec)

	updateMonitorStateCmd := m.UpdateMonitorStateCommand{
		Id:      job.MonitorId,
		State:   res,
		Updated: job.LastPointTs,
		Checked: preExec,
	}
	if err := bus.Dispatch(&updateMonitorStateCmd); err != nil {
		//check if we failed due to deadlock.
		if err.Error() == "Error 1213: Deadlock found when trying to get lock; try restarting transaction" {
			err = bus.Dispatch(&updateMonitorStateCmd)
		}
	}
	if err != nil {
		return fmt.Errorf("non-fatal: failed to update monitor state: %q", err)
	}
	if updateMonitorStateCmd.Affected > 0 {
		//emit a state change event.
		if job.Notifications.Enabled {
			emails := strings.Split(job.Notifications.Addresses, ",")
			if len(emails) < 1 {
				log.Debug("no email addresses provided. OrgId: %d monitorId: %d", job.OrgId, job.MonitorId)
			} else {
				sendCmd := m.SendEmailCommand{
					To:       emails,
					Template: "alerting_notification.html",
					Data: map[string]interface{}{
						"EndpointId":   job.EndpointId,
						"EndpointName": job.EndpointName,
						"EndpointSlug": job.EndpointSlug,
						"Settings":     job.Settings,
						"CheckType":    job.MonitorTypeName,
						"State":        res.String(),
						"TimeLastData": job.LastPointTs, // timestamp of the most recent data used
						"TimeExec":     preExec,         // when we executed the alerting rule and made the determination
					},
				}

				if err := bus.Dispatch(&sendCmd); err != nil {
					log.Info("failed to send email to %s. OrgId: %d monitorId: %d", emails, job.OrgId, job.MonitorId, err)
				}
			}
		}
	}
	//store the result in graphite.
	job.StoreResult(res)

	// the bosun api abstracts parsing, execution and graphite querying for us via 1 call.
	// we want to have some individual times
	if gr, ok := gr.(*graphite.GraphiteContext); ok {
		executorJobQueryGraphite.Value(gr.Dur)
		executorJobParseAndEval.Value(durationExec - gr.Dur)
		if gr.MissingVals > 0 {
			executorGraphiteMissingVals.Value(int64(gr.MissingVals))
		}
		if gr.EmptyResp != 0 {
			executorGraphiteEmptyResponse.Inc(int64(gr.EmptyResp))
		}
	}

	switch res {
	case m.EvalResultOK:
		executorAlertOutcomesOk.Inc(1)
	case m.EvalResultWarn:
		executorAlertOutcomesWarn.Inc(1)
	case m.EvalResultCrit:
		executorAlertOutcomesCrit.Inc(1)
	case m.EvalResultUnknown:
		executorAlertOutcomesUnkn.Inc(1)
	}

	return nil
}
