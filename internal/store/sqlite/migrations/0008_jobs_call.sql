-- The jobs a call started, looked up by the call's device, dispatch boot and
-- call id when a session call settles lost. job_id gives ListJobs' order, so
-- the planner prefers this index over the primary key.
CREATE INDEX jobs_call ON jobs (device_id, boot_id, call_id, job_id);
