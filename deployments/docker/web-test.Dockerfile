# Frontend-only test deployment. Context must be web/dist after `npm --prefix web run build`.
# Reuses the validated test backend so this UI rollout does not change its database/runtime.
# docker build -f deployments/docker/web-test.Dockerfile --build-arg BASE_IMAGE=fasttask:gantt-test-20261110 \
#   --build-arg WEB_REVISION=$(git rev-parse HEAD) -t fasttask:workbench-test-20261110 web/dist
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
ARG WEB_REVISION
LABEL io.fastrd.fasttask.web-revision=${WEB_REVISION}
USER root
RUN rm -rf /opt/fasttask/web/dist
COPY . /opt/fasttask/web/dist/
USER fasttask
