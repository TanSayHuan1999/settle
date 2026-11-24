FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/settle ./cmd/settle

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/settle /settle
EXPOSE 8080
ENTRYPOINT ["/settle"]
