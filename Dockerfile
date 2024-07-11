FROM golang:1.22

WORKDIR /go/src/sigs.k8s.io/scheduler-plugins
COPY . .

RUN make build

FROM alpine

COPY --from=0 /go/src/sigs.k8s.io/scheduler-plugins/bin/my-scheduler /bin/kube-scheduler

WORKDIR /bin
CMD ["kube-scheduler"]
