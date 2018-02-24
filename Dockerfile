#FROM        prom/prometheus:v2.0.0
FROM        debian:stretch-slim
MAINTAINER  GuoLiangShuai

ENV RULES_DIR   ""
ENV PROMETHEUS_URL ""

COPY pod-alert  /bin/pod-alert
COPY promtool   /bin/promtool

CMD  /bin/pod-alert -logtostderr=true
# ENTRYPOINT [ "/bin/pod-alert" ]
