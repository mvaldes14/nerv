# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/nerv ./cmd/nerv

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nerv /nerv
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/nerv"]
CMD ["serve", "--addr", ":8080"]
