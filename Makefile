.PHONY: build deploy

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -o=bin/my-scheduler ./cmd/scheduler

buildLocal:
	docker build . -t mike911209/my-scheduler:latest
	docker push mike911209/my-scheduler:latest

loadImage:
	kind load docker-image my-scheduler:local

deploy:
	helm install scheduler-plugins charts/ --create-namespace --namespace scheduler-plugins

remove:
	helm uninstall -n scheduler-plugins scheduler-plugins

clean:
	rm -rf bin/
