FROM        prom/prometheus:v2.0.0
MAINTAINER  GuoLiangShuai

ENV RULES_DIR ""
ENV PROMETHEUS_URL ""

COPY pod-alert  /bin/pod-alert
COPY promtool   /bin/promtool

ENTRYPOINT [ "/bin/pod-alert" ]
