FROM fedora
RUN yum -y install etcd golang mongodb

ADD mongoconf.go /
RUN go build mongoconf.go

ADD conf.sh /
CMD /conf.sh

