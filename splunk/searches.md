# Splunk searches for deploy events

Events arrive with `sourcetype=pushbutton:deploy`. Each event is the JSON the controller emits: `type`, `target`, `version`, `detail`, plus `deployment` when the REST service sends them.

These searches are written against that shape. They have not been run against a real Splunk instance, only the sender has been tested, against a stand-in collector that checks the HEC envelope, auth header, batching and retries.

Every deploy and how it ended:

```
sourcetype=pushbutton:deploy (type=done OR type=rollback_done OR type=rollback_failed)
| stats latest(_time) as finished by deployment type
| eval outcome=case(type="done","succeeded", type="rollback_done","rolled back", type="rollback_failed","ROLLBACK FAILED")
| table finished deployment outcome
```

Which targets fail most often, and at which step:

```
sourcetype=pushbutton:deploy type=target_failed
| rex field=detail "^(?<step>drain|update|verify|enable):"
| stats count by target step
| sort - count
```

How long one deploy took, from the first event to the last:

```
sourcetype=pushbutton:deploy
| stats earliest(_time) as start latest(_time) as end by deployment
| eval seconds=round(end-start,1)
| sort - start
```

Canary verdicts and the reason given:

```
sourcetype=pushbutton:deploy (type=canary_pass OR type=canary_fail)
| table _time deployment type detail
```

Alert: a rollback did not complete and someone has to act (trigger on any result):

```
sourcetype=pushbutton:deploy type=rollback_failed earliest=-15m
```

Alert: a deploy has been running for more than 20 minutes without finishing:

```
sourcetype=pushbutton:deploy earliest=-2h
| stats earliest(_time) as start values(type) as types by deployment
| where NOT (mvfind(types,"^done$") >= 0 OR mvfind(types,"^rollback_done$") >= 0 OR mvfind(types,"^rollback_failed$") >= 0)
| where now()-start > 1200
```

Correlate a deploy with what users saw: join the deploy window to the application's 5xx count (the `access_combined` sourcetype and `status` field here are placeholders for whatever your access logs use):

```
index=web sourcetype=access_combined status>=500
| timechart span=30s count as errors
```

Overlay it on the deploy timeline in a dashboard panel to see whether errors moved when a batch went out.
