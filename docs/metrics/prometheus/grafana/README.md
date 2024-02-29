# How to monitor MinIO server with Grafana [![Slack](https://slack.min.io/slack?type=svg)](https://slack.min.io)

[Grafana](https://grafana.com/) allows you to query, visualize, alert on and understand your metrics no matter where they are stored. Create, explore, and share dashboards with your team and foster a data driven culture.

## Prerequisites

- Prometheus and MinIO configured as explained in [document here](https://github.com/minio/minio/blob/master/docs/metrics/prometheus/README.md).
- Grafana installed as explained [here](https://grafana.com/grafana/download).

## MinIO Grafana Dashboard

Visualize MinIO metrics with our official Grafana dashboard available on the [Grafana dashboard portal](https://grafana.com/grafana/dashboards/13502).

Refer to the dashboard [json file here](https://raw.githubusercontent.com/minio/minio/master/docs/metrics/prometheus/grafana/minio-dashboard.json).

![Grafana](https://raw.githubusercontent.com/minio/minio/master/docs/metrics/prometheus/grafana/grafana-minio.png)

Replication metrics can be viewed in the Grafana dashboard using [json file here](https://raw.githubusercontent.com/minio/minio/master/docs/metrics/prometheus/grafana/replication/minio-replication.json)

![Grafana](https://raw.githubusercontent.com/minio/minio/master/docs/metrics/prometheus/grafana/replication/grafana-replication.png)

Bucket metrics can be viewed in the Grafana dashboard using [json file here](https://raw.githubusercontent.com/minio/minio/master/docs/metrics/prometheus/grafana/bubcket/minio-bucket.json)

![Grafana](https://raw.githubusercontent.com/minio/minio/master/docs/metrics/prometheus/grafana/bucket/grafana-bucket.png)

Note: All these dashboards are provided as an example and need basis they should be customized as well as new graphs should be added.
