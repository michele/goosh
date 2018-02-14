FROM iron/go
COPY goosh-linux-amd64 /goosh
EXPOSE 8080
CMD ["/goosh"]
